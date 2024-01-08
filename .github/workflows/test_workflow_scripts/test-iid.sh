# Add fake installation-id for the workflow.
sudo mkdir ~/.keploy-config
sudo touch ~/.keploy-config/installation-id.yaml
echo "ObjectID('123456789')" | sudo tee ~/.keploy-config/installation-id.yaml > /dev/null