#!/bin/bash
sudo WORKSPACE=bruckins-ea7578d ./k8shelld --config config.yaml --port 2830 --socket /tmp/test.sock --cert server.crt --key server.key --test
