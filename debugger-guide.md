
# Debugger Guide 👨‍🔧

Here we will learn how to use debugger for Keploy in VS Code.

To bring up the **Run and Debug** view, select the **Run and Debug** icon in the **Activity Bar** on the side of VS Code. You can also use the keyboard shortcut `Cmd + Shift + D` for MacOS and `Ctrl + shift + D` for Windows.

If running and debugging is not yet configured (no `launch.json` has been created), VS Code shows the Run start view.

![config-debug](https://media.discordapp.net/attachments/1166595840505294908/1207236720647471134/image.png?ex=65dee9cf&is=65cc74cf&hm=4986ddca66cbc03ff7301ced02c819b7d82a1a3705584883271b1b2b9eb91a69&=&format=webp&quality=lossless)

Click on **create a launch.json file**  👉  **Go**  👉  **Go: Launch Package**

Navigate to `launch.json` to begin crafting JSON objects.

```json
{
    "version": "0.2.0",
    "configurations": [
        {
            "name": "Record",
            "type": "go",
            "request": "launch",
            "mode": "auto",
            "asRoot": true,
            "console": "integratedTerminal",
            "program": "main.go",
            "args": [
                "record",
                "-c",
                "<path_to_executable>"
            ],
            "env": {
                "PATH": "<path to Go binary>"
            }
        },
        {
            "name": "Test",
            "type": "go",
            "request": "launch",
            "mode": "auto",
            "asRoot": true,
            "console": "integratedTerminal",
            "program": "main.go",
            "args": [
                "test",
                "-c",
                "<path_to_executable>", // We can also use docker commands here
            ],
            "env": {
                "PATH": "<path to Go binary>"
            }
        }
    ]
}

```

Let's take a closer look at some important key-value pairs in our JSON file:

- The `"name"`  parameter can be anything, but for convenience, consider using the keploy command name (e.g., `Record` and `Test`).
- The `args` parameter represents the entire keploy command to run your application. For example, in [Gin + Mongo](https://keploy.io/docs/quickstart/samples-gin/), on Linux, it's `record -c "go run main.go handler.go"`, and for MacOS, it's `record -c "docker compose up" --containerName "ginMongoApp"`.

You can either add more objects in the "configurations" array or modify the "args" property within a single object to add more commands.

```json
{
    "name": "Test",
    "type": "go",
    "request": "launch",
    "mode": "auto",
    "asRoot": true,
    "console": "integratedTerminal",
    "program": "main.go",
    "args": [
        "test",
        "-c",
        "<path_to_executable>",
    ],
    "env": {
        "PATH": "<path to Go binary>"
    }
},
{
    "name": "Test-debug",
    "type": "go",
    "request": "launch",
    "mode": "auto",
    "asRoot": true,
    "console": "integratedTerminal",
    "program": "main.go",
    "args": [
        "test",
        "-c",
        "<path_to_executable>",
        "--debug"
    ],
    "env": {
        "PATH": "<path to Go binary>"
    }
}
```

> **Note**: By default, the keploy tests and reports will be generated in the keploy directory. You can change this by creating the [generate-config](https://keploy.io/docs/running-keploy/cli-commands/#generate-config) and specifying the desired path.

## Running the Debugger

Click the **Start Debugging** button to witness the magic of debugging unfold seamlessly.