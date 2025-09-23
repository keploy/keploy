# Add fake installation-id for the workflow. Be idempotent on repeated runs.
mkdir -p "$HOME/.keploy"
printf "%s\n" "ObjectID('123456789')" > "$HOME/.keploy/installation-id.yaml"