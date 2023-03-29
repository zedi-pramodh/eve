#!/bin/sh
#
# Copyright (c) 2023 Zededa, Inc.
# SPDX-License-Identifier: Apache-2.0

K3S_VERSION=v1.25.3+k3s1
KUBEVIRT_VERSION=v0.58.0
LONGHORN_VERSION=v1.4.0

INSTALL_LOG=/var/log/install.log
# Check if k3s is already installed.
date >> $INSTALL_LOG

setup_cgroup () {
        echo "cgroup /sys/fs/cgroup cgroup defaults 0 0" >> /etc/fstab
}
network_available=false                                               
                       
check_network_connection () {
case "$(curl -s --max-time 5 -I http://google.com | sed 's/^[^ ]*  *\([0-9]\).*/\1/; 1q')" in
  [23]) network_available=true;;                                                             
  5) echo "The web proxy won't let us through" >> $INSTALL_LOG;;                             
  *) echo "The network is down" >> $INSTALL_LOG;;                               
esac                                                            
}                                                               
                                                              
#Is this ZFS block device ?
if [ -b /dev/zvol/persist/clustered-storage ]; then
	mount /dev/zvol/persist/clustered-storage /var/lib  ## This is where we persist the cluster components (k3s containers)
fi

#Prereqs
modprobe tun                                                        
modprobe vhost_net    
modprobe fuse         
/etc/init.d/iscsid start
mount --make-rshared /

while true;
do         
if [ ! -d /var/lib/longhorn ]; then
        check_network_connection                 
        if [ $network_available = "false" ]; then
                echo "Waiting for network connectivity" >> $INSTALL_LOG
        else                                                           
		setup_cgroup                                                   
		echo "Installing K3S version $K3S_VERSION" >> $INSTALL_LOG
		nohup /usr/bin/k3s server  --cluster-init --log=/var/log/k3s.log  &                                
		sleep 30 
		echo "Installing Kubevirt version $KUBEVIRT_VERSION" >> $INSTALL_LOG                                       
		kubectl apply -f https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}/kubevirt-operator.yaml
		kubectl apply -f https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}/kubevirt-cr.yaml      
		
		echo "Installing longhorn version $LONGHORN_VERSION" >> $INSTALL_LOG                                       
                kubectl apply -f https://raw.githubusercontent.com/longhorn/longhorn/${LONGHORN_VERSION}/deploy/longhorn.yaml
                
        fi                                                                                                                
else                                                                                                                      
	ps -ef | grep "k3s server" | grep -v "grep" >> $INSTALL_LOG 
        if [ $? -eq 0 ]; then
        	echo "k3s is alive " >> $INSTALL_LOG                                                               
        else
		## Must be after reboot
		echo "Starting k3s server " >> $INSTALL_LOG
		nohup /usr/bin/k3s server --log=/var/log/k3s.log  &                                
        fi
fi                                                   
        sleep 15                                     
done 
