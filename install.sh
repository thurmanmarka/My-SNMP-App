#!/bin/bash
# Installation script for My-SNMP-App (template)
# Place this in the My-SNMP-App directory and customize as needed

set -e

INSTALL_USER="myhomeservices"
INSTALL_DIR="/home/$INSTALL_USER/snmp-app"
SERVICE_NAME="snmp-app"

# Check if running as correct user
if [ "$USER" != "$INSTALL_USER" ]; then
    echo "Warning: This script is designed to run as the '$INSTALL_USER' user."
    read -p "Do you want to continue anyway? (y/n) " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        echo "Please run as the $INSTALL_USER user or create the user first."
        exit 1
    fi
fi

# Build the binary for ARM (Raspberry Pi)
echo "Building binary for ARM..."
GOOS=linux GOARCH=arm GOARM=7 go build -o snmp-app .

# Create installation directory
mkdir -p $INSTALL_DIR
cp snmp-app $INSTALL_DIR/
cp config.yaml $INSTALL_DIR/

# Install systemd service (template, user must provide .service file)
# sudo cp snmp-app.service /etc/systemd/system/
# sudo systemctl daemon-reload

# Enable and start service
# sudo systemctl enable snmp-app
# sudo systemctl start snmp-app

echo "\nMy-SNMP-App installation complete!"
echo "(Customize this script as needed for your deployment.)"
