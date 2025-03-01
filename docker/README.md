# How to use the lava docker images

## Building lava docker images

1. Download the lava sources:
    git clone https://github.com/lavanet/lava.git

2. Build the lava docker image locally
    # to build from the current checked-out code:
    make docker-build

    # to build a specific lava version
    LAVA_BUILD_OPTIONS="release" LAVA_VERSION=0.4.3 make docker-build

The result would be a docker image names `lava` tagged with the version.
For example the output of `docker images` after the above:
    lava                               0.4.3                 bc3a85c7623f   2 hours ago      256MB
    lava                               latest                bc3a85c7623f   2 hours ago      256MB
    lava                               0.4.3-a5e1202-dirty   5ff644084c3d   2 hours ago      257MB

## Running lava containers with docker

**TODO**

## Running lava containers with docker-compose

**Run Lava Node**

1. Review the settings in `docker/env`. The default settings are usually
suitable for all deployments.

2. Use the following the commands to create/start/stop/destroy the node:

To start the node:
    docker-compose --profile node --env-file docker/env -f docker/docker-compose.yml up

To stop/start the node:
    docker-compose --profile node --env-file docker/env -f docker/docker-compose.yml stop
    docker-compose --profile node --env-file docker/env -f docker/docker-compose.yml start

To destroy the node:
    docker-compose --profile node --env-file docker/env -f docker/docker-compose.yml down

**Run Lava Portal \ Provider**

1. Create a lava user and fund it.

    export LAVA_HOME='.lava'
    export LAVA_USER='my-user'

    # create a new user, and then show its address
    build/lavad keys add $LAVA_USER --home $LAVA_HOME --keyring-backend test
    build/lavad keys list --home $LAVA_HOME --keyring-backend test list

    LAVA_ADDR=$(lavad keys show "${LAVA_USER}" --home $LAVA_HOME --keyring-backend test | \
        grep address | awk '{print $2}')

    # fund the new user: see https://docs.lavanet.xyz/faucet

    # verify the user has funds
    build/lavad query bank balances $LAVA_ADDR --home $LAVA_HOME --denom ulava \
        --node http://public-rpc.lavanet.xyz:80/rpc/
    
2. Review the settings in `docker/env`. Fill in all the mandatory values
for the 'provider' role.

3. Use the following the commands to create/start/stop/destroy the node (for
'provider' replace the role 'portal' with 'provider'):

To start the portal/provider:
    docker-compose --profile portal --env-file docker/env -f docker/docker-compose.yml up

To stop/start the portal/provider:
    docker-compose --profile portal --env-file docker/env -f docker/docker-compose.yml stop
    docker-compose --profile portal --env-file docker/env -f docker/docker-compose.yml start

To destroy the portal/provider:
    docker-compose --profile portal --env-file docker/env -f docker/docker-compose.yml down

