#!/usr/bin/env bash

set -e


sudo docker buildx create --name mybuilder --use --bootstrap 

sudo docker buildx build --push --platform linux/amd64,linux/arm64 --tag puding/expenseowl:0.4 --tag puding/expenseowl:latest .

sudo docker buildx rm mybuilder
