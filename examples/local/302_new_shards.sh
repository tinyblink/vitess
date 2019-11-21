#!/bin/bash

# Copyright 2019 The Vitess Authors.
# 
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
# 
#     http://www.apache.org/licenses/LICENSE-2.0
# 
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# this script brings up new tablets for the two new shards that we will
# be creating in the customer keyspace and copies the schema 

set -e

# shellcheck disable=SC2128
script_root=$(dirname "${BASH_SOURCE}")

SHARD=-80 CELL=zone1 KEYSPACE=customer UID_BASE=300 "$script_root/vttablet-up.sh"
SHARD=80- CELL=zone1 KEYSPACE=customer UID_BASE=400 "$script_root/vttablet-up.sh"

./lvtctl.sh InitShardMaster -force customer/-80 zone1-300
./lvtctl.sh InitShardMaster -force customer/80- zone1-400
./lvtctl.sh CopySchemaShard customer/0 customer/-80
./lvtctl.sh CopySchemaShard customer/0 customer/80-

disown -a
