#!/bin/bash

if [ "$1" = "deploy" ]; then
    go run main.go deploy
elif [ "$1" = "destroy" ]; then
    go run main.go destroy
else
    echo "Usage: $0 [deploy|destroy]"
    exit 1
fi



