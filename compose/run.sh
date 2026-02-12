#!/bin/bash

set -ex

pushd ..
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o fs ./cmd/fs
popd

docker compose down --volumes --timeout 1
docker compose up --build --detach --remove-orphans

sleep 1

# create bucket "tempo"
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_EC2_METADATA_DISABLED=true
aws --endpoint-url=http://localhost:8080 s3 mb s3://tempo
