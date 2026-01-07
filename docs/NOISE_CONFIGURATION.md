# Noise Configuration Guide

## Overview

Noise configuration in Keploy allows you to ignore specific fields or entire response/request bodies during test replay while still validating the overall structure and other fields.

This is useful when:
- Response contains dynamic values (timestamps, UUIDs, request IDs)
- You want to validate JSON structure but ignore all values
- Certain fields change between test runs but don't affect functionality

## Configuration Structure

Noise configuration is defined in `keploy.yml` under `test.globalNoise`:

```yaml
test:
  globalNoise:
    global:
      response:
        body:
          # Define noise rules here
        header:
          # Define header noise rules here
    test-sets:
      test-set-name:
        response:
          body:
            # Test-set-specific noise rules
```

## Usage Patterns

### 1. **Ignore Entire Response Body** (Recommended for Many Fields)

Validates the response is valid JSON but ignores all field values:

```yaml
global:
  response:
    body:
      "*": ["*"]
```

**Use case:** Endpoint returns 50-60 fields with many dynamic values

**What it does:**
- ✅ Validates JSON structure exists
- ✅ Ignores all field value differences
- ✅ Single configuration line for entire body

---

### 2. **Ignore Specific Fields Globally**

Ignores a specific field name at ANY depth in the JSON:

```yaml
global:
  response:
    body:
      timestamp: [".*"]
      id: [".*"]
      uuid: [".*"]
      createdAt: [".*"]
      updatedAt: [".*"]
```

**Use case:** Multiple endpoints return same dynamic fields you want to ignore

**What it does:**
- ✅ Matches field names anywhere in JSON (any nesting level)
- ✅ Values in matching fields are ignored
- ✅ Other fields are still compared

**Example:**
```json
{
  "user": {
    "id": 123,  // ← ignored (matches "id")
    "name": "John"
  },
  "id": "abc-123",  // ← ignored (matches "id")
  "timestamp": "2024-01-07T10:00:00Z"  // ← ignored
}
```

---

### 3. **Ignore Fields at Specific Paths**

Ignores fields only at specified nested paths:

```yaml
global:
  response:
    body:
      "user.id": [".*"]           # Ignore id under user object
      "data.timestamp": [".*"]     # Ignore timestamp under data object
      "items[*].status": [".*"]    # Ignore status in array elements
```

**Use case:** Same field name means different things at different locations

**What it does:**
- ✅ Only ignores fields at specified paths
- ✅ Same field at different paths NOT ignored
- ✅ Fine-grained control

**Example:**
```json
{
  "user": {
    "id": 123,      // ← ignored (matches "user.id")
    "name": "John"
  },
  "product": {
    "id": 456       // ← NOT ignored (path is "product.id" not "user.id")
  }
}
```

---

### 4. **Regular Expression Matching**

Use regex patterns to match values:

```yaml
global:
  response:
    body:
      requestId: ["^req-[0-9]+$"]  # Match request ID pattern
      token: ["^Bearer .*"]         # Match bearer tokens
      email: [".*@example\\.com$"]  # Match emails from domain
```

**Use case:** Validate format of dynamic values without checking exact value

---

## Header Noise Configuration

Same patterns work for request/response headers:

```yaml
global:
  response:
    header:
      "Content-Length": [".*"]      # Ignore content length header
      "X-Request-ID": [".*"]        # Ignore request ID header
```

---

## Common Mistakes and Solutions

### ❌ Wrong: Using JSONPath syntax

```yaml
body:
  "$..*": [".*"]      # JSONPath not supported - will fail
  "$..timestamp": [".*"]
```

**Fix:** Use supported syntax
```yaml
body:
  timestamp: [".*"]    # Global field match
  # OR for entire body:
  "*": ["*"]
```

---

### ❌ Wrong: Using regex for field matching

```yaml
body:
  ".*": [".*"]        # Regex patterns for field names not supported
  "time_.*": [".*"]   # This won't match "timestamp"
```

**Fix:** Use exact field names or paths
```yaml
body:
  timestamp: [".*"]
  createdAt: [".*"]
  # OR for entire body:
  "*": ["*"]
```

---

### ❌ Wrong: Incorrect path format

```yaml
body:
  "user->id": [".*"]   # Wrong separator
  "user/id": [".*"]    # Wrong separator
  "user.id.name": [".*"]  # Can specify exact path only
```

**Fix:** Use dot notation for paths
```yaml
body:
  "user.id": [".*"]      # ✅ Correct
  "user.name": [".*"]    # ✅ Correct
```

---

## Test-Set Specific Configuration

Override global noise for specific test sets:

```yaml
test:
  globalNoise:
    global:
      response:
        body:
          timestamp: [".*"]
    
    test-sets:
      auth-tests:
        response:
          body:
            token: [".*"]           # Additional noise for auth-tests
            sessionId: [".*"]
      
      user-tests:
        response:
          body:
            "*": ["*"]              # Ignore entire body for user-tests
```

---

## Examples

### Example 1: E-commerce Product API

```yaml
global:
  response:
    body:
      id: [".*"]                    # Ignore product ID
      createdAt: [".*"]            # Ignore creation timestamp
      updatedAt: [".*"]            # Ignore last update
      "inventory.lastRestocked": [".*"]  # Ignore restock time
```

### Example 2: User Service with Dynamic Fields

```yaml
global:
  response:
    body:
      "*": ["*"]  # Ignore all values, just validate structure
```

### Example 3: Mixed Configuration

```yaml
global:
  response:
    body:
      # Validate these fields strictly
      "user.email": ["^[a-z0-9._%+-]+@[a-z0-9.-]+\\.[a-z]{2,}$"]
      
      # Ignore these fields
      timestamp: [".*"]
      uuid: [".*"]
      
    header:
      "Content-Length": [".*"]      # Ignore size (may vary)
      "X-Request-ID": [".*"]        # Ignore request tracking ID
```

---

## Validation and Debugging

Keploy will warn you about unsupported patterns:

```
WARN: JSONPath syntax is not supported in noise configuration
Note: Supported formats: (1) '*' for entire body, (2) exact field names (3) dot-separated paths like 'user.id'
```

Enable debug logs to see which fields are being ignored:

```bash
keploy test --debug
```

---

## FAQ

**Q: Can I use wildcards like `user.*` to match all fields under user?**
A: No, use exact path notation like `"user.id"`, `"user.name"`, etc. Or use the global field name approach.

**Q: Does noise affect mock matching?**
A: Yes, noise is applied during both test response matching and mock matching.

**Q: Can I combine different noise types?**
A: Yes! You can use global field names, specific paths, and entire body ignore together.

**Q: What happens if a field in noise config doesn't exist?**
A: It's simply ignored (no error). Noise config is additive - only fields that exist and match are ignored.

---

## Best Practices

1. **Start broad, be specific if needed**
   - Use `"*": ["*"]` if most fields are dynamic
   - Switch to specific field names if few fields are dynamic

2. **Document why fields are noisy**
   - Add comments explaining timestamp, UUID, etc.

3. **Use test-set specific noise sparingly**
   - Most noise should be global for consistency

4. **Regularly review noise configuration**
   - Ensure you're not ignoring fields you actually need to validate

5. **Test your configuration**
   - Run tests and verify they pass as expected
   - Check that structure validation still works
