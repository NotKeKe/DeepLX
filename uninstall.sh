#!/bin/bash

# Colors
red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

# Check for root privileges
if [[ $EUID -ne 0 ]]; then
    echo -e "${red}This script must be run as root.${plain}"
    exit 1
fi

uninstall_dlx() {
    echo -e "${green}Starting DLX uninstallation...${plain}"

    # 1. Stop and disable the DLX service
    if systemctl is-active --quiet dlx; then
        echo -e "${yellow}Stopping DLX service...${plain}"
        systemctl stop dlx
    else
        echo -e "${yellow}DLX service is not running or not found.${plain}"
    fi

    if systemctl is-enabled --quiet dlx; then
        echo -e "${yellow}Disabling DLX service from starting on boot...${plain}"
        systemctl disable dlx
    else
        echo -e "${yellow}DLX service is not enabled.${plain}"
    fi

    # 2. Remove the systemd service file
    if [ -f /etc/systemd/system/dlx.service ]; then
        echo -e "${yellow}Removing DLX systemd service file (/etc/systemd/system/dlx.service)...${plain}"
        rm -f /etc/systemd/system/dlx.service
        systemctl daemon-reload
        echo -e "${green}Systemd daemon reloaded.${plain}"
    else
        echo -e "${yellow}DLX systemd service file not found, skipping removal.${plain}"
    fi

    # 3. Remove the DLX executable
    if [ -f /usr/bin/dlx ]; then
        echo -e "${yellow}Removing DLX executable (/usr/bin/dlx)...${plain}"
        rm -f /usr/bin/dlx
    else
        echo -e "${yellow}DLX executable not found, skipping removal.${plain}"
    fi

    echo -e "${green}DLX uninstallation complete.${plain}"
    echo -e "${green}If you wish to reinstall, please run the install script again.${plain}"
}

uninstall_dlx
