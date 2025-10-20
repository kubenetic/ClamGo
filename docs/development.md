# Development

In this section, we will describe how to setup a development environment.

## Start ClamD as container for virus scanning

```bash
mkdir -p /tmp/clamav/{scandir,database,clamd}
chown -R 100:101 /tmp/clamav
chmod -R 770 /tmp/clamav

docker run --detach \
    --name=clamav \
    --mount type=bind,source=/tmp/clamav/scandir/,target=/scandir/ \
    --mount type=bind,source=/tmp/clamav/database/,target=/var/lib/clamav \
    --mount type=bind,source=/tmp/clamav/clamd/,target=/tmp/ \
    docker.io/clamav/clamav:latest
```

## RabbitMQ as container for queuing


