# Add fake installation-id for the workflow.
mkdir ~/.keploy
touch ~/.keploy/installation-id.yaml
echo "ObjectID('123456789')" | tee ~/.keploy/installation-id.yaml > /dev/null