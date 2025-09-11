# keploy contract Command

The `keploy contract` command is used to manage service contracts in Keploy. Contracts help define and validate the expected behavior of services, enabling better testing and integration.

## Usage

```
keploy contract <subcommand> [flags]
```

### Subcommands

#### generate
Generate contract(s) for specified services.

**Usage:**
```
keploy contract generate --service="email,notify"
```

**Description:**
Generates contracts for the services specified in the `--service` flag.

#### download
Download contract(s) for specified services to a local path.

**Usage:**
```
keploy contract download --service="email,notify" --path /local/path
```

**Description:**
Downloads contracts for the specified services to the given path.

#### test
Validate contract(s) for specified services.

**Usage:**
```
keploy contract test --service="email,notify" --path /local/path
```

**Description:**
Validates the contracts for the specified services, ensuring they meet the expected definitions.

## Flags
- `--service` (required): Comma-separated list of service names.
- `--path` (optional): Local path for download or validation.

## Examples
- Generate contracts:
  ```
  keploy contract generate --service="email,notify"
  ```
- Download contracts:
  ```
  keploy contract download --service="email,notify" --path ./contracts
  ```
- Validate contracts:
  ```
  keploy contract test --service="email,notify" --path ./contracts
  ```

---
For more details, visit the [Keploy CLI documentation](https://keploy.io/docs/running-keploy/cli-commands/).
