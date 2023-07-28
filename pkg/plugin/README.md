# Plugin Package Documentation

This package is responsible for launching the plugin processes 
and managing the gRPC connections. An instance is created for each 
plugin, with methods that are utilized by other packages. Designed 
as an independent module, it operates separately from the others.