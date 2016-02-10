#!/bin/sh

touch /usr/share/docker/plugins/macvlan.sock
docker-compose up -d
