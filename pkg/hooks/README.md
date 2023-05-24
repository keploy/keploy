# Hooks Package Documentation

Hooks package contains the user space go code to load the eBPF hooks
and maps to instrument the user API. This package is used by the CLI 
commands. Also it launches the proxies on range of local ports to 
capture the egress calls.