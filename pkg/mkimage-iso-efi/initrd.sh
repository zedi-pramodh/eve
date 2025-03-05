#!/bin/sh

# Copyright (c) 2024 Zededa, Inc.
# SPDX-License-Identifier: Apache-2.0

# Mount necessary filesystems
mount -t proc none /proc
mount -t sysfs none /sys
mount -t devtmpfs none /dev

# debug enabled?
# shellcheck disable=SC2002
debug_param=$(cat /proc/cmdline | tr ' ' '\n' | grep '^debug$' | head -n 1)
if [ -n "$debug_param" ]; then
    set -x
fi


# Search for the root= cmdline property
# shellcheck disable=SC2002
root_param=$(cat /proc/cmdline | tr ' ' '\n' | grep '^root=' | head -n 1)
# remove the leading "root="  to get the actual value
root_value=${root_param#root=}

# Search for the rootimg= cmdline property
# shellcheck disable=SC2002
rootimg_param=$(cat /proc/cmdline | tr ' ' '\n' | grep '^rootimg=' | head -n 1)
# remove the leading "root="  to get the actual value
root_img=${rootimg_param#rootimg=}

# Check if root_value is set
if [ -z "$root_value" ]; then
    echo "Error: No root= parameter found in /proc/cmdline"
    exec sh
fi

echo "searching for root filesystem with value: $root_value"

rootdev=""

# Some emulated CD/DVD-ROM devices might take some time to appear in the
# system, set a maximum number of retries (one per second) until give up
cnt=10
while [ "$cnt" -gt 0 ]; do
    # Determine if the root_value is a LABEL, UUID, or direct device path
    while read -r line; do
        case "$root_value" in
            LABEL=*)
                label=${root_value#LABEL=}
                if echo "$line" | grep -q "LABEL=\"$label\""; then
                    rootdev=$(echo "$line" | cut -d: -f1)
                    break
                fi
                ;;
            UUID=*)
                uuid=${root_value#UUID=}
                if echo "$line" | grep -q "UUID=\"$uuid\""; then
                    rootdev=$(echo "$line" | cut -d: -f1)
                    break
                fi
                ;;
            PARTUUID=*)
                partuuid=${root_value#PARTUUID=}
                if echo "$line" | grep -q "PARTUUID=\"$partuuid\""; then
                    rootdev=$(echo "$line" | cut -d: -f1)
                    break
                fi
                ;;
            *)
                rootdev="$root_value"
                ;;
        esac
    done <<EOF
$(blkid)
EOF

    if [ -n "$rootdev" ]; then
        break
    else
        echo "Waiting for root device... "
        sleep 1
        cnt=$((cnt - 1))
    fi
done

# If root filesystem is found, mount it
if [ -n "$rootdev" ]; then
    echo "found root filesystem: $rootdev, switching"
    mount "$rootdev" /newroot
    # Now, check for the installer rootfs squashfs image
    if [ -n "$root_img" ]; then
        rootfsimg=/newroot/"$root_img"
        if [ -e "$rootfsimg" ]; then
            # Mount the image and call switch_root
            mkdir -p /installer_root
            mount "$rootfsimg" /installer_root
            exec switch_root /installer_root /sbin/init
        else
            echo "$root_img image not found!"
            exec sh
        fi
    else
        # No image provided, let's just switch root
        exec switch_root /newroot /sbin/init
    fi
else
    echo "Root filesystem not found!"
    exec sh
fi
