package tools

// GitleaksDefaultConfig contains the default gitleaks rules configuration
// This is kept separate for maintainability - adding new rules is as simple as
// updating this TOML configuration string
const GitleaksDefaultConfig = `
title = "gitleaks config"

[[rules]]
    id = "aws-access-key"
    description = "AWS Access Key"
    regex = '''(A3T[A-Z0-9]|AKIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ASIA)[A-Z0-9]{16}'''
    tags = ["key", "AWS"]

[[rules]]
    id = "aws-secret-key"
    description = "AWS Secret Key"
    regex = '''(?i)aws(.{0,20})?(?-i)['\"][0-9a-zA-Z\/+]{40}['\"]'''
    tags = ["key", "AWS"]

[[rules]]
    id = "aws-mws-key"
    description = "AWS MWS key"
    regex = '''amzn\.mws\.[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}'''
    tags = ["key", "AWS", "MWS"]

[[rules]]
    id = "github-pat"
    description = "Github Personal Access Token"
    regex = '''ghp_[0-9a-zA-Z]{36}'''
    tags = ["key", "Github"]

[[rules]]
    id = "github-oauth"
    description = "Github OAuth Access Token"
    regex = '''gho_[0-9a-zA-Z]{36}'''
    tags = ["key", "Github"]

[[rules]]
    id = "github-app-token"
    description = "Github App Token"
    regex = '''(ghu|ghs)_[0-9a-zA-Z]{36}'''
    tags = ["key", "Github"]

[[rules]]
    id = "github-refresh-token"
    description = "Github Refresh Token"
    regex = '''ghr_[0-9a-zA-Z]{76}'''
    tags = ["key", "Github"]

[[rules]]
    id = "github-fine-grained-pat"
    description = "Found a GitHub Fine-Grained Personal Access Token, risking unauthorized repository access and code manipulation."
    regex = '''github_pat_[0-9a-zA-Z_]{82}'''
    tags = ["key", "github-fine-grained-pat"]

[[rules]]
    id = "slack-app-token"
    description = "Detected a Slack App-level token, risking unauthorized access to Slack applications and workspace data."
    regex = '''(?i)(xapp-\d-[A-Z0-9]+-\d+-[a-z0-9]+)'''
    tags = ["key", "slack-app-token"]
    
[[rules]]
    id = "slack-bot-token"
    description = "Identified a Slack Bot token, which may compromise bot integrations and communication channel security."
    regex = '''(xoxb-[0-9]{10,13}\-[0-9]{10,13}[a-zA-Z0-9-]*)'''
    tags = ["key", "slack-bot-token"]
    
[[rules]]
    id = "slack-config-access-token"
    description = "Found a Slack Configuration access token, posing a risk to workspace configuration and sensitive data access."
    regex = '''(?i)(xoxe.xox[bp]-\d-[A-Z0-9]{163,166})'''
    tags = ["key", "slack-config-access-token"]
    
[[rules]]
    id = "slack-config-refresh-token"
    description = "Discovered a Slack Configuration refresh token, potentially allowing prolonged unauthorized access to configuration settings."
    regex = '''(?i)(xoxe-\d-[A-Z0-9]{146})'''
    tags = ["key", "slack-config-refresh-token"]
    
[[rules]]
    id = "slack-legacy-bot-token"
    description = "Uncovered a Slack Legacy bot token, which could lead to compromised legacy bot operations and data exposure."
    regex = '''(xoxb-[0-9]{8,14}\-[a-zA-Z0-9]{18,26})'''
    tags = ["key", "slack-legacy-bot-token"]
    
[[rules]]
    id = "slack-legacy-token"
    description = "Detected a Slack Legacy token, risking unauthorized access to older Slack integrations and user data."
    regex = '''(xox[os]-\d+-\d+-\d+-[a-fA-F\d]+)'''
    tags = ["key", "slack-legacy-token"]
    
[[rules]]
    id = "slack-legacy-workspace-token"
    description = "Identified a Slack Legacy Workspace token, potentially compromising access to workspace data and legacy features."
    regex = '''(xox[ar]-(?:\d-)?[0-9a-zA-Z]{8,48})'''
    tags = ["key", "slack-legacy-workspace-token"]
    
[[rules]]
    id = "slack-user-token"
    description = "Found a Slack User token, posing a risk of unauthorized user impersonation and data access within Slack workspaces."
    regex = '''(xox[pe](?:-[0-9]{10,13}){3}-[a-zA-Z0-9-]{28,34})'''
    tags = ["key", "slack-user-token"]
    
[[rules]]
    id = "slack-webhook-url"
    description = "Discovered a Slack Webhook, which could lead to unauthorized message posting and data leakage in Slack channels."
    regex = '''(https?:\/\/)?hooks.slack.com\/(services|workflows)\/[A-Za-z0-9+\/]{43,46}'''
    tags = ["key", "slack-webhook-url"]

[[rules]]
    id = "snyk-api-token"
    description = "Uncovered a Snyk API token, potentially compromising software vulnerability scanning and code security."
    regex = '''(?i)(?:snyk_token|snyk_key|snyk_api_token|snyk_api_key|snyk_oauth_token)(?:[0-9a-z\-_\t .]{0,20})(?:[\s|']|[\s|"]){0,3}(?:=|>|:{1,3}=|\|\|:|<=|=>|:|\?=)(?:'|\"|\s|=|\x60){0,5}([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\b'''
    tags = ["key", "snyk-api-token"]

[[rules]]
    id = "asymmetric-private-key"
    description = "Asymmetric Private Key"
    regex = '''-----BEGIN ((EC|PGP|DSA|RSA|OPENSSH) )?PRIVATE KEY( BLOCK)?-----'''
    tags = ["key", "AsymmetricPrivateKey"]

[[rules]]
    id = "google-api-key"
    description = "Google API key"
    regex = '''AIza[0-9A-Za-z\\-_]{35}'''
    tags = ["key", "Google"]

[[rules]]
    id = "heroku-api-key"
    description = "Heroku API key"
    regex = '''(?i)heroku(.{0,20})?[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}'''
    tags = ["key", "Heroku"]

[[rules]]
    id = "mailchimp-api-key"
    description = "MailChimp API key"
    regex = '''(?i)(mailchimp|mc)(.{0,20})?[0-9a-f]{32}-us[0-9]{1,2}'''
    tags = ["key", "Mailchimp"]

[[rules]]
    id = "mailgun-api-key"
    description = "Mailgun API key"
    regex = '''((?i)(mailgun|mg)(.{0,20})?)?key-[0-9a-z]{32}'''
    tags = ["key", "Mailgun"]

[[rules]]
    id = "paypal-braintree-token"
    description = "PayPal Braintree access token"
    regex = '''access_token\$production\$[0-9a-z]{16}\$[0-9a-f]{32}'''
    tags = ["key", "Paypal"]

[[rules]]
    id = "picatic-api-key"
    description = "Picatic API key"
    regex = '''sk_live_[0-9a-z]{32}'''
    tags = ["key", "Picatic"]

[[rules]]
    id = "sendgrid-api-key"
    description = "SendGrid API Key"
    regex = '''SG\.[\w_]{16,32}\.[\w_]{16,64}'''
    tags = ["key", "SendGrid"]

[[rules]]
    id = "stripe-api-key"
    description = "Stripe API key"
    regex = '''(?i)stripe(.{0,20})?[sr]k_live_[0-9a-zA-Z]{24}'''
    tags = ["key", "Stripe"]

[[rules]]
    id = "square-access-token"
    description = "Square access token"
    regex = '''sq0atp-[0-9A-Za-z\-_]{22}'''
    tags = ["key", "square"]

[[rules]]
    id = "square-oauth-secret"
    description = "Square OAuth secret"
    regex = '''sq0csp-[0-9A-Za-z\\-_]{43}'''
    tags = ["key", "square"]

[[rules]]
    id = "twilio-api-key"
    description = "Twilio API key"
    regex = '''(?i)twilio(.{0,20})?SK[0-9a-f]{32}'''
    tags = ["key", "twilio"]

[[rules]]
    id = "dynatrace-token"
    description = "Dynatrace ttoken"
    regex = '''dt0[a-zA-Z]{1}[0-9]{2}\.[A-Z0-9]{24}\.[A-Z0-9]{64}'''
    tags = ["key", "Dynatrace"]

[[rules]]
    id = "pypi-upload-token"
    description = "PyPI upload token"
    regex = '''pypi-AgEIcHlwaS5vcmc[A-Za-z0-9-_]{50,1000}'''
    tags = ["key", "pypi"]
    
[[rules]]
    id = "datadog-access-token"
    description = "Detected a Datadog Access Token, potentially risking monitoring and analytics data exposure and manipulation."
    regex = '''(?i)(?:datadog)(?:[0-9a-z\-_\t .]{0,20})(?:[\s|']|[\s|"]){0,3}(?:=|>|:{1,3}=|\|\|:|<=|=>|:|\?=)(?:'|\"|\s|=|\x60){0,5}([a-z0-9]{40})\b'''
    tags = ["key", "datadog-access-token"]

[[rules]]
    id = "npm-access-token"
    description = "Uncovered an npm access token, potentially compromising package management and code repository access."
    regex = '''(?i)\b(npm_[a-z0-9]{36})\b'''
    tags = ["key", "npm-access-token"]

[[rules]]
    id = "jwt"
    description = "Uncovered a JSON Web Token, which may lead to unauthorized access to web applications and sensitive user data."
    regex = '''\b(ey[a-zA-Z0-9]{17,}\.ey[a-zA-Z0-9\/\\_-]{17,}\.(?:[a-zA-Z0-9\/\\_-]{10,}={0,2})?)\b'''
    tags = ["key", "jwt"]
    
[[rules]]
    id = "jwt-base64"
    description = "Detected a Base64-encoded JSON Web Token, posing a risk of exposing encoded authentication and data exchange information."
    regex = '''\bZXlK(?:(?P<alg>aGJHY2lPaU)|(?P<apu>aGNIVWlPaU)|(?P<apv>aGNIWWlPaU)|(?P<aud>aGRXUWlPaU)|(?P<b64>aU5qUWlP)|(?P<crit>amNtbDBJanBi)|(?P<cty>amRIa2lPaU)|(?P<epk>bGNHc2lPbn)|(?P<enc>bGJtTWlPaU)|(?P<jku>cWEzVWlPaU)|(?P<jwk>cWQyc2lPb)|(?P<iss>cGMzTWlPaU)|(?P<iv>cGRpSTZJ)|(?P<kid>cmFXUWlP)|(?P<key_ops>clpYbGZiM0J6SWpwY)|(?P<kty>cmRIa2lPaUp)|(?P<nonce>dWIyNWpaU0k2)|(?P<p2c>d01tTWlP)|(?P<p2s>d01uTWlPaU)|(?P<ppt>d2NIUWlPaU)|(?P<sub>emRXSWlPaU)|(?P<svt>emRuUWlP)|(?P<tag>MFlXY2lPaU)|(?P<typ>MGVYQWlPaUp)|(?P<url>MWNtd2l8)|(?P<use>MWMyVWlPaUp)|(?P<ver>MlpYSWlPaU)|(?P<version>MlpYSnphVzl1SWpv)|(?P<x>NElqb2)|(?P<x5c>NE5XTWlP)|(?P<x5t>NE5YUWlPaU)|(?P<x5ts256>NE5YUWpVekkxTmlJNkl)|(?P<x5u>NE5YVWlPaU)|(?P<zip>NmFYQWlPaU))[a-zA-Z0-9\/\\_+\-\r\n]{40,}={0,2}'''
    tags = ["key", "jwt-base64"]

[[rules]]
    id = "jwt-in-param-escape-aware"
    description = "JWT in URL/JSON param (handles =, \\u003d and &, \\u0026, %26)"
    regex = '''(?i)(?:token|id_token|access_token|auth_token|jwt)(?:=|\\u003d)([A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})(?:&|\\u0026|%26|["'\s]|$)'''
    tags = ["jwt", "escape-aware"]

[[rules]]
    id = "auth-bearer-header-escape-aware"
    description = "Authorization: Bearer <token> (handles :, \\u003a, =, \\u003d)"
    regex = '''(?i)authorization(?:\s*(?::|\\u003a|=|\\u003d))\s*bearer\s+([A-Za-z0-9._~-]{20,})'''
    tags = ["header", "escape-aware"]

[[rules]]
    id = "hdnts-hmac"
    description = "Signed URL (hdnts) HMAC parameter"
    regex = '''(?i)hdnts(?:=|\\u003d)[^"\s]{0,512}?~hmac=([A-Fa-f0-9]{32,64})'''
    tags = ["signed-url", "hmac"]

[[rules]]
    id = "long-token-in-param-escape-aware"
    description = "Opaque token in URL/JSON param (handles =, \\u003d and &, \\u0026, %26)"
    regex = '''(?i)(?:token|access_token|auth_token|session_token|sig|signature|apikey|api_key)(?:=|\\u003d)([A-Za-z0-9._~%-]{32,})(?:&|\\u0026|%26|["'\s]|$)'''
    tags = ["token", "escape-aware"]
`
