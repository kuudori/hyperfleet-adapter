# HyperFleet API Client

Pure HTTP client for communicating with the HyperFleet API. Supports configurable timeouts, retry logic with multiple backoff strategies, and a clean functional options pattern.

## Features

- **Pure HTTP client**: No dependencies on config_loader or other internal packages
- **Configurable timeout**: Set HTTP request timeout per-client or per-request
- **Retry logic**: Automatic retry with configurable attempts
- **Backoff strategies**: Exponential, linear, or constant backoff with jitter
- **Functional options**: Clean configuration pattern for both client and requests
- **Response helpers**: Methods to check success, error status, and retryability

## Usage

### Basic Usage

```go
import "github.com/openshift-hyperfleet/hyperfleet-adapter/internal/hyperfleetapi"

// Create a client with defaults
client := hyperfleet_api.NewClient()

// Make a GET request
ctx := context.Background()
resp, err := client.Get(ctx, "https://api.hyperfleet.io/v1/clusters/123")
if err != nil {
    log.Fatal(err)
}

if resp.IsSuccess() {
    fmt.Printf("Response: %s\n", resp.Body)
}
```

### With Client Options

```go
// Create client with custom configuration
client := hyperfleet_api.NewClient(
    hyperfleet_api.WithTimeout(30*time.Second),
    hyperfleet_api.WithRetryAttempts(5),
    hyperfleet_api.WithRetryBackoff(hyperfleet_api.BackoffExponential),
    hyperfleet_api.WithBaseDelay(500*time.Millisecond),
    hyperfleet_api.WithMaxDelay(30*time.Second),
    hyperfleet_api.WithDefaultHeader("X-Custom", "value"),
)
```

### Request Options

```go
// Override per-request settings
resp, err := client.Get(ctx, url,
    hyperfleet_api.WithRequestTimeout(60*time.Second),
    hyperfleet_api.WithRequestRetryAttempts(10),
    hyperfleet_api.WithHeader("X-Request-ID", requestID),
)

// POST with JSON body - Content-Type defaults to application/json
body, _ := json.Marshal(payload)
resp, err := client.Post(ctx, url, body)

// Override Content-Type for non-JSON payloads
resp, err := client.Post(ctx, url, xmlBody,
    hyperfleet_api.WithHeader("Content-Type", "application/xml"),
)
```

> **Note:** When a request has a body, `Content-Type` defaults to `application/json` if not explicitly set.

### Using with Adapter Config (in message handler)

```go
// In your message handler, parse config and create client:
func createAPIClient(apiConfig config_loader.HyperfleetAPIConfig) (hyperfleet_api.Client, error) {
    var opts []hyperfleet_api.ClientOption

    // Set base URL if configured (env fallback handled in NewClient)
    if apiConfig.BaseURL != "" {
        opts = append(opts, hyperfleet_api.WithBaseURL(apiConfig.BaseURL))
    }

    // Set timeout if configured (0 means use default)
    if apiConfig.Timeout > 0 {
        opts = append(opts, hyperfleet_api.WithTimeout(apiConfig.Timeout))
    }

    // Set retry attempts
    if apiConfig.RetryAttempts > 0 {
        opts = append(opts, hyperfleet_api.WithRetryAttempts(apiConfig.RetryAttempts))
    }

    // Validate and set retry backoff strategy
    if apiConfig.RetryBackoff != "" {
        switch apiConfig.RetryBackoff {
        case hyperfleet_api.BackoffExponential, hyperfleet_api.BackoffLinear, hyperfleet_api.BackoffConstant:
            opts = append(opts, hyperfleet_api.WithRetryBackoff(apiConfig.RetryBackoff))
        default:
            return nil, fmt.Errorf("invalid retry backoff strategy %q (supported: exponential, linear, constant)", apiConfig.RetryBackoff)
        }
    }

    if apiConfig.BaseDelay > 0 {
        opts = append(opts, hyperfleet_api.WithBaseDelay(apiConfig.BaseDelay))
    }

    if apiConfig.MaxDelay > 0 {
        opts = append(opts, hyperfleet_api.WithMaxDelay(apiConfig.MaxDelay))
    }

    for key, value := range apiConfig.DefaultHeaders {
        opts = append(opts, hyperfleet_api.WithDefaultHeader(key, value))
    }

    return hyperfleet_api.NewClient(opts...), nil
}
```

## Client Options

| Option | Description |
|--------|-------------|
| `WithTimeout(d)` | Set HTTP client timeout |
| `WithRetryAttempts(n)` | Set number of retry attempts |
| `WithRetryBackoff(b)` | Set backoff strategy |
| `WithBaseDelay(d)` | Set initial retry delay |
| `WithMaxDelay(d)` | Set maximum retry delay |
| `WithDefaultHeader(k, v)` | Add default header to all requests |
| `WithConfig(c)` | Set full ClientConfig |
| `WithHTTPClient(c)` | Use custom http.Client |

## Request Options

| Option | Description |
|--------|-------------|
| `WithHeader(k, v)` | Add header to request |
| `WithHeaders(m)` | Add multiple headers |
| `WithBody(b)` | Set raw request body; preserves any explicitly set Content-Type header, or falls back to `application/json` at request time if none provided |
| `WithJSONBody(b)` | Set body and immediately set `Content-Type: application/json` in request headers (semantic alias making JSON intent explicit) |
| `WithRequestTimeout(d)` | Override timeout for this request |
| `WithRequestRetryAttempts(n)` | Override retry attempts |
| `WithRequestRetryBackoff(b)` | Override backoff strategy |

### Body Options: `WithBody` vs `WithJSONBody`

Both options produce identical HTTP requests when sending JSON, but differ in **when** the Content-Type header is set:

```go
// WithBody: sets body only; Content-Type applied later by client fallback
resp, _ := client.Post(ctx, url, nil,
    WithBody(jsonBytes),
)
// Content-Type: application/json (set by client default at request time)

// WithJSONBody: sets body AND Content-Type immediately in the option
resp, _ := client.Post(ctx, url, nil,
    WithJSONBody(jsonBytes),
)
// Content-Type: application/json (set explicitly by the option)

// WithBody with explicit override for non-JSON payloads
resp, _ := client.Post(ctx, url, nil,
    WithBody(xmlBytes),
    WithHeader("Content-Type", "application/xml"),
)
// Content-Type: application/xml (explicit header takes precedence)
```

**When to use which:**
- Use `WithBody` for general-purpose body setting, especially when you may override Content-Type
- Use `WithJSONBody` when you want self-documenting code that clearly signals JSON intent

> **Rationale:** Both exist for API ergonomics. `WithJSONBody` makes code intent explicit at the call site, while `WithBody` provides flexibility for non-JSON payloads. They are functionally equivalent for JSON since the client defaults to `application/json` anyway.

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `HYPERFLEET_API_BASE_URL` | Base URL for the HyperFleet API | - |
| `HYPERFLEET_API_VERSION` | API version | `v1` |

## Backoff Strategies

| Strategy | Description | Example (base=1s) |
|----------|-------------|-------------------|
| `BackoffExponential` | Doubles delay each retry | 1s, 2s, 4s, 8s... |
| `BackoffLinear` | Increases delay linearly | 1s, 2s, 3s, 4s... |
| `BackoffConstant` | Same delay between retries | 1s, 1s, 1s, 1s... |

All strategies include ±10% jitter to prevent thundering herd problems.

## Retryable Status Codes

The client treats **all 5xx responses** plus the following client errors as retryable:
- `408` Request Timeout
- `429` Too Many Requests

Common retryable server errors include:
- `500` Internal Server Error
- `502` Bad Gateway
- `503` Service Unavailable
- `504` Gateway Timeout

Other 4xx status codes are not retried.

## Response Helpers

```go
resp.IsSuccess()     // true for 2xx status codes
resp.IsClientError() // true for 4xx status codes
resp.IsServerError() // true for 5xx status codes
resp.IsRetryable()   // true for retryable status codes

resp.StatusCode      // HTTP status code
resp.Body            // Response body as []byte
resp.Duration        // Total request duration including retries
resp.Attempts        // Number of attempts made
```
