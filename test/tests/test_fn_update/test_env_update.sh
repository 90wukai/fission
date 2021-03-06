#!/bin/bash

set -euo pipefail

source $(dirname $0)/fnupdate_utils.sh

ROOT=$(dirname $0)/../../..

env_old=python-$(date +%N)
env_new=python-$(date +%N)
fn=hellopy-$(date +%N)

log "Creating env $env_old"
fission env create --name $env_old --image fission/python-env
trap "fission env delete --name $env_old" EXIT

log "Creating function $fn"
fission fn create --name $fn --env $env_old --code $ROOT/examples/python/hello.py --minscale 1 --maxscale 4 --executortype newdeploy --mincpu 20 --maxcpu 100 --minmemory 128 --maxmemory 256
trap "fission fn delete --name $fn" EXIT

log "Creating route for function $fn"
fission route create --function ${fn} --url /${fn} --method GET

log "Waiting for router to catch up"
sleep 5

timeout 60 bash -c "test_fn $fn 'world'"

log "Creating a new env $env_new"
fission env create --name $env_new --image fission/python-env
trap "fission env delete --name $env_old" EXIT

log "Updating function with a new environment"
fission fn update --name $fn --env $env_new --code $ROOT/examples/python/hello.py --minscale 1 --maxscale 4 --executortype newdeploy --mincpu 20 --maxcpu 100 --minmemory 128 --maxmemory 256

log "Waiting for update to catch up"
sleep 5

timeout 60 bash -c "test_fn $fn 'world'"