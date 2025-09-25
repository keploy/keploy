# Add fake installation-id for the workflow.
if [ -d "$HOME/.keploy" ]; then
  echo "~/.keploy already exists, skipping installation-id setup."
else
  mkdir -p "$HOME/.keploy"
  touch "$HOME/.keploy/installation-id.yaml"
  echo "ObjectID('123456789')" > "$HOME/.keploy/installation-id.yaml"
fi
