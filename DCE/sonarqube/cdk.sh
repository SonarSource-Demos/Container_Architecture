#!/bin/bash

# Check if both parameters are provided
if [ $# -ne 2 ]; then
    echo "Usage: $0 [deploy|destroy] [namespace]"
    exit 1
fi

# Check if the first parameter is "deploy" or "destroy"
if [ "$1" != "deploy" ] && [ "$1" != "destroy" ]; then
     echo "Usage: $0 [deploy|destroy] [namespace]" 
    exit 1
fi

# Perform actions based on the first parameter
if [ "$1" == "deploy" ]; then
    echo "Deploying SonarQube DCE in namespace $2..."
    echo "Create Namespace $2"
	if kubectl get namespace "$2" &> /dev/null; then
    		echo "Namespace '$2' already exists... please choose another namespace name or delete this one"
    		exit 1
	else
    		kubectl create namespace "$2"
    		echo "Namespace '$2' created."
                export JWT_SECRET="IdxtucwIbaMzrv0R18kLq7DgsipCJZPKj96jxyTE1o8=" 
                cd ../helm-chart-sonarqube/charts
                echo "Deploying SonarQube DCE HELM Chart"
                helm upgrade --install -n $2 sonarqubedce02 --set ApplicationNodes.jwtSecret=$JWT_SECRET ./sonarqube-dce/ -f values.yaml 
    		exit 0
	fi
    # Action for deploying
elif [ "$1" == "destroy" ]; then
    echo "Destroying $2..."
    # Action for destroying
fi

