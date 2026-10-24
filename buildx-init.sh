#!/usr/bin/env bash

set -e

VERSION="${1:?usage: $0 <version, e.g. 0.5>}"


sudo docker buildx create --name mybuilder --use --bootstrap 

sudo docker buildx build --push --platform linux/amd64,linux/arm64 --build-arg VERSION="$VERSION" --tag "puding/expenseowl:$VERSION" --tag puding/expenseowl:latest .

sudo docker buildx rm mybuilder
