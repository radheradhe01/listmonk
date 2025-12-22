1. **Update Database**: Change `app.root_url` from `http://localhost:9000` to `https://email.crownsolutions.com` to satisfy Gmail's header filters.
2. **Modify** **`internal/messenger/email/email.go`**:

   * Explicitly set the `Return-Path` header to match the authenticated sender.

   * Add a check to strip or fix any remaining `localhost` strings in headers.
3. **Restart & Test**: Restart the backend and re-run campaign ID 50. Confirm in logs that `List-Unsubscribe` and other headers no longer contain `localhost`.

