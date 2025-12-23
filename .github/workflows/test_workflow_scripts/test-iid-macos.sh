# Add fake installation-id for the workflow.
if [ -d "$HOME/.keploy" ]; then
  echo "~/.keploy already exists, skipping installation-id setup."
else
  mkdir -p "$HOME/.keploy"
  touch "$HOME/.keploy/installation-id.yaml"
  echo "ObjectID('123456789')" > "$HOME/.keploy/installation-id.yaml"
fi

# Ensure JOB_ID is set for container/network isolation on shared runners
# If JOB_ID is not set by the workflow, generate a unique one
if [ -z "${JOB_ID:-}" ]; then
  export JOB_ID="$(date +%s)-$$"
  echo "Warning: JOB_ID was not set, using generated ID: $JOB_ID"
fi
echo "Using JOB_ID: $JOB_ID"
