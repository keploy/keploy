package cmd

import (
    "fmt"
    "github.com/spf13/cobra"
    // ⬇️ THIS LINE MUST BE CHANGED ⬇️
    "github.com/keploy/keploy/pkg/update" 
)

// KeployVersion is a variable that holds the version of the currently running binary.
// In a production build, this is set using linker flags (ldflags).
// For development, we set a default value like "v0.0.0-dev".
var KeployVersion string = "v0.0.0-dev" 

// NewUpdateCmd creates the Cobra command structure for 'keploy update'.
func NewUpdateCmd() *cobra.Command {
    return &cobra.Command{
        Use:   "update",
        Short: "Checks for and updates Keploy to the latest stable version.",
        Long:  `The update command fetches the latest stable binary for your system and replaces the current executable.`,
        Run: func(cmd *cobra.Command, args []string) {
            
            fmt.Println("Starting Keploy update check...")
            
            // ⭐️ Call the core update function from the pkg/update package
            if err := update.UpdateCLI(KeployVersion); err != nil {
                fmt.Printf("🔴 Error during update: %v\n", err)
            } else {
                fmt.Println("✅ Keploy update process finished.")
            }
        },
    }
}