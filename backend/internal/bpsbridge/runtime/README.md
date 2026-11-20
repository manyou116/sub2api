# Embedded BPS runtime

Go packages adapted from [bps-local-public at 8d9e164](https://github.com/manyou116/bps-local-public/tree/8d9e164571eea63cc322717337de82f8dce6e219).

The embedding imports these packages through `internal/bpsbridge`.
The CLI, browser capture, standalone credentials discovery and token
refresh are outside the integration. Sub2API supplies a verified bound
grant and owns the admin and account lifecycle.
