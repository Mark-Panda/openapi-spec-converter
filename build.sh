#!/usr/bin/env bash

go run cmd/openapi-spec-converter/main.go -t swagger -f json \
    < openapi.yaml \
    > openapi-swagger.json
