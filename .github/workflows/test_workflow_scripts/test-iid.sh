# Add fake installation-id for the workflow.
sudo mkdir ~/.keploy
sudo touch ~/.keploy/installation-id.yaml
echo "ObjectID('123456789')" | sudo tee ~/.keploy/installation-id.yaml > /dev/null