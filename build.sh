#!/usr/bin/env bash

go run cmd/openapi-spec-converter/main.go -t swagger -f json \                              ✔  192.168.3.57 IP
    < openapi.yaml \
    > openapi-swagger.json
