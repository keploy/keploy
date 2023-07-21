# Hooks Package Documentation

The `hooks` package contains the user-space Go code responsible for 
loading eBPF hooks and eBPF maps, which are used to instrument the user 
API. This package is utilized by the CLI commands. Additionally, it 
launches proxies on a range of local ports to capture egress calls.