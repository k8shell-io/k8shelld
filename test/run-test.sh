#!/bin/bash
../bin/k8shelld --config config.yaml --port 2830 --socket /tmp/test.sock --server-cert server.crt --server-key server.key --test
