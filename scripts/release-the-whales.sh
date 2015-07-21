#!/bin/bash

# Launch a bunch of busybox containers > release-the-whales.sh 50
# Delete all busybox containers > release-the-whales.sh clean

image_name="busybox"

delcon() {
    # if no containers matching $image_name exist you will get an error of:
    # docker: "rm" requires a minimum of 1 argument.
    echo "Deleting all Busybox containers..."
    docker rm -f $(docker ps  -a | grep $image_name | awk '{print $1}')
}

if [ "$1" -eq "$1" ] 2>/dev/null; then
    echo "Release the whales!"
    for i in `seq 1 $1`;
    do
        echo "Launching container #$i"
        docker run -itd --name=container-$i busybox
    done
fi

if [[ $1 = "clean" ]]; then
    delcon
elif [[ $1 = "" ]]; then
    echo "Supports 2 options:"
    echo "==================="
    echo "1) Launch (n) of busybox containers. Example:[ release-the-whales.sh 20 ]"
    echo "2) Delete all containers matching [ $image_name ] Example:[ release-the-whales.sh clean ]"
fi
