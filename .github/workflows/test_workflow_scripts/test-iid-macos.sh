# Do NOT use bare `mkdir` (use -p), and avoid sudo so ~ expands to the runner's HOME.
install -d -m 700 "$HOME/.keploy"
: > "$HOME/.keploy/installation-id.yaml"   # truncate or create
printf "ObjectID('123456789')\n" > "$HOME/.keploy/installation-id.yaml"
