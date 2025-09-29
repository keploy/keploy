# Add a fake installation-id for the workflow.

# In PowerShell, $HOME is the equivalent of '~' for the user's home directory.
# Join-Path is a robust way to create file paths that works across systems.
$keployDir = Join-Path $HOME ".keploy"
$filePath = Join-Path $keployDir "installation-id.yaml"

# Create the .keploy directory if it doesn't already exist.
# -Force prevents an error if the directory already exists.
# `| Out-Null` suppresses the output to keep the logs clean.
New-Item -Path $keployDir -ItemType Directory -Force | Out-Null

# Set-Content creates the file if it doesn't exist and writes the content to it.
# This replaces both the `touch` and `echo | tee` commands.
# `sudo` is not needed as GitHub Windows runners have sufficient permissions.
Set-Content -Path $filePath -Value "ObjectID('123456789')"

Write-Host "Created fake installation-id at $filePath"
