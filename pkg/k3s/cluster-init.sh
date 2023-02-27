#!/bin/sh
#
# Copyright (c) 2018 Zededa, Inc.
# SPDX-License-Identifier: Apache-2.0

K3S_VERSION=v1.25.3+k3s1
KUBEVIRT_VERSION=v0.58.0
LONGHORN_VERSION=

INSTALL_LOG=/var/log/install.log
# Check if k3s is already installed.
date >> $INSTALL_LOG

setup_cgroup () {
        echo "cgroup /sys/fs/cgroup cgroup defaults 0 0" >> /etc/fstab
}
network_available=false                                               
                       
check_network_connection () {
case "$(curl -s --max-time 2 -I http://google.com | sed 's/^[^ ]*  *\([0-9]\).*/\1/; 1q')" in
  [23]) network_available=true;;                                                             
  5) echo "The web proxy won't let us through" >> $INSTALL_LOG;;                             
  *) echo "The network is down or very slow" >> $INSTALL_LOG;;                               
esac                                                            
}                                                               
                                                              
while true;
do         
if [ ! -f /usr/local/bin/k3s-uninstall.sh ]; then
        check_network_connection                 
        if [ $network_available = "false" ]; then
                echo "Waiting for network connectivity" >> $INSTALL_LOG
        else                                                           
		setup_cgroup                                                   
		echo "Installing K3S version $K3S_VERSION" >> $INSTALL_LOG
		/usr/bin/curl -sfL https://get.k3s.io | INSTALL_K3S_SKIP_ENABLE=true INSTALL_K3S_VERSION=${K3S_VERSION} sh - 
		nohup /usr/local/bin/k3s server  --snapshotter=native --log=/var/log/k3s.log  &                                
		sleep 15 
		echo "Installing Kubevirt version $KUBEVIRT_VERSION" >> $INSTALL_LOG                                       
		#apk add libvirt                                                                                            
		#apk add libvirt-client                                                  
		modprobe tun                                                        
		modprobe vhost_net    
		modprobe fuse         
		#kubectl apply -f https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}/kubevirt-operator.yaml
		#kubectl apply -f https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}/kubevirt-cr.yaml      
        fi                                                                                                                
else                                                                                                                      
	ps -ef | grep "k3s server" | grep -v "grep" >> $INSTALL_LOG 
        if [ $? -eq 0 ]; then
        	echo "k3s is alive " >> $INSTALL_LOG                                                               
        else
		## Must be after reboot
		echo "Starting k3s server " >> $INSTALL_LOG
		nohup /usr/local/bin/k3s server  --snapshotter=native --log=/var/log/k3s.log  &                                
        fi
fi                                                   
        sleep 15                                     
done 
