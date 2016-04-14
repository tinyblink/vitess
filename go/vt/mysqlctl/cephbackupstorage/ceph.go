// Package Cephbackupstorage implements the BackupStorage interface
// for Ceph Cloud Storage.
package cephbackupstorage

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/minio/minio-go"
	"github.com/youtube/vitess/go/vt/concurrency"
	"github.com/youtube/vitess/go/vt/mysqlctl/backupstorage"
)

var (
	// bucket is where the backups will go.
	bucket string
	// configFilePath is where the configs/credentials for backups will be stored.
	ceph_backup_storage_config = flag.String("ceph_config_location", "ceph_backup_config.json",
		"Path to JSON config file for ceph backup storage")
)

var StorageConfig struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	EndPoint  string `json:"endPoint"`
	Bucket    string `json:"bucket"`
}

// CephBackupHandle implements BackupHandle for Ceph Cloud Storage.
type CephBackupHandle struct {
	client    *minio.Client
	bs        *CephBackupStorage
	dir       string
	name      string
	readOnly  bool
	errors    concurrency.AllErrorRecorder
	waitGroup sync.WaitGroup
}

// Directory implements BackupHandle.
func (bh *CephBackupHandle) Directory() string {
	return bh.dir
}

// Name implements BackupHandle.
func (bh *CephBackupHandle) Name() string {
	return bh.name
}

// AddFile implements BackupHandle.
func (bh *CephBackupHandle) AddFile(filename string) (io.WriteCloser, error) {
	if bh.readOnly {
		return nil, fmt.Errorf("AddFile cannot be called on read-only backup")
	}
	reader, writer := io.Pipe()
	bh.waitGroup.Add(1)
	go func() {
		defer bh.waitGroup.Done()
		// Give PutObject() the read end of the pipe.
		object := objName(bh.dir, bh.name, filename)
		_, err := bh.client.PutObject(bucket, object, reader, "application/octet-stream")
		if err != nil {
			// Signal the writer that an error occurred, in case it's not done writing yet.
			reader.CloseWithError(err)
			// In case the error happened after the writer finished, we need to remember it.
			bh.errors.RecordError(err)
		}
	}()
	// Give our caller the write end of the pipe.
	return writer, nil
}

// EndBackup implements BackupHandle.
func (bh *CephBackupHandle) EndBackup() error {
	if bh.readOnly {
		return fmt.Errorf("EndBackup cannot be called on read-only backup")
	}
	bh.waitGroup.Wait()
	// Return the saved PutObject() errors, if any.
	return bh.errors.Error()
}

// AbortBackup implements BackupHandle.
func (bh *CephBackupHandle) AbortBackup() error {
	if bh.readOnly {
		return fmt.Errorf("AbortBackup cannot be called on read-only backup")
	}
	return bh.bs.RemoveBackup(bh.dir, bh.name)
}

// ReadFile implements BackupHandle.
func (bh *CephBackupHandle) ReadFile(filename string) (io.ReadCloser, error) {
	if !bh.readOnly {
		return nil, fmt.Errorf("ReadFile cannot be called on read-write backup")
	}
	object := objName(bh.dir, bh.name, filename)
	return bh.client.GetObject(bucket, object)
}

// CephBackupStorage implements BackupStorage for Ceph Cloud Storage.
type CephBackupStorage struct {
	// client is the instance of the Ceph Cloud Storage Go client.
	// Once this field is set, it must not be written again/unset to nil.
	_client *minio.Client
	// mu guards all fields.
	mu sync.Mutex
}

// ListBackups implements BackupStorage.
func (bs *CephBackupStorage) ListBackups(dir string) ([]backupstorage.BackupHandle, error) {
	c, err := bs.client()
	if err != nil {
		return nil, err
	}

	// List prefixes that begin with dir (i.e. list subdirs).
	var subdirs []string
	searchPrefix := objName(dir, "")

	doneCh := make(chan struct{})
	for object := range c.ListObjects(bucket, searchPrefix, false, doneCh) {
		if object.Err != nil {
			return nil, object.Err
		}
		subdir := strings.TrimPrefix(object.Key, searchPrefix)
		subdir = strings.TrimSuffix(subdir, "/")
		subdirs = append(subdirs, subdir)
	}

	// Backups must be returned in order, oldest first.
	sort.Strings(subdirs)

	result := make([]backupstorage.BackupHandle, 0, len(subdirs))
	for _, subdir := range subdirs {
		result = append(result, &CephBackupHandle{
			client:   c,
			bs:       bs,
			dir:      dir,
			name:     subdir,
			readOnly: true,
		})
	}
	return result, nil
}

// StartBackup implements BackupStorage.
func (bs *CephBackupStorage) StartBackup(dir, name string) (backupstorage.BackupHandle, error) {
	c, err := bs.client()
	if err != nil {
		return nil, err
	}

	return &CephBackupHandle{
		client:   c,
		bs:       bs,
		dir:      dir,
		name:     name,
		readOnly: false,
	}, nil
}

// RemoveBackup implements BackupStorage.
func (bs *CephBackupStorage) RemoveBackup(dir, name string) error {
	c, err := bs.client()
	if err != nil {
		return err
	}
	fullName := objName(dir, name, "")
	//	err = c.RemoveObject(bucket, fullName)
	//      if err != nil {
	//              return err
	//      }
	//      return nil
	var arr []string
	doneCh := make(chan struct{})
	defer close(doneCh)
	for object := range c.ListObjects(bucket, fullName, true, doneCh) {
		if object.Err != nil {
			return object.Err
		}
		arr = append(arr, object.Key)
	}
	for _, obj := range arr {
		err = c.RemoveObject(bucket, obj)
		if err != nil {
			log.Fatalln(err)
		}
	}
	return nil
}

// Close implements BackupStorage.
func (bs *CephBackupStorage) Close() error {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	if bs._client != nil {
		// a new client the next time one is needed.
		bs._client = nil
	}
	return nil
}

// client returns the Ceph Storage client instance.
// If there isn't one yet, it tries to create one.
func (bs *CephBackupStorage) client() (*minio.Client, error) {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	if bs._client == nil {
		configFile, err := os.Open(*ceph_backup_storage_config)
		if err != nil {
			return nil, fmt.Errorf("file not present : %v", err)
		}
		defer configFile.Close()
		jsonParser := json.NewDecoder(configFile)
		if err = jsonParser.Decode(&StorageConfig); err != nil {
			return nil, fmt.Errorf("Error parsing the json file : %v", err)
		}

		bucket = StorageConfig.Bucket
		accessKey := StorageConfig.AccessKey
		secretKey := StorageConfig.SecretKey
		url := StorageConfig.EndPoint

		ceph_client, err := minio.NewV2(url, accessKey, secretKey, true)
		if err != nil {
			return nil, err
		}
		bs._client = ceph_client
	}
	return bs._client, nil
}

func init() {
	backupstorage.BackupStorageMap["ceph"] = &CephBackupStorage{}
}

// objName joins path parts into an object name.
// Unlike path.Join, it doesn't collapse ".." or strip trailing slashes.
// It also adds the value of the -gcs_backup_storage_root flag if set.
func objName(parts ...string) string {
	return strings.Join(parts, "/")
}
