#!/bin/bash

set -e

dt=$(date '+%Y%m%d%H%M')
label=v1.13.1
image=mlixytz/k8s-host-device-plugin:$label-$dt
echo "build $image"
docker build . -t $image
echo "start push..."
docker push $image
echo "push done"
