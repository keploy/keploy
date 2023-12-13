# Add fake installation-id to the config file.
sudo mkdir ~/.keploy-config
sudo touch ~/.keploy-config/installation-id.yaml
sudo echo "ObjectID('123456789')" > ~/.keploy-config/installation-id.yaml