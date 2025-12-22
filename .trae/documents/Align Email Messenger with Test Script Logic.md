I will align the Listmonk email messenger implementation with the successful `test_gmail.go` logic.

The key discrepancies to resolve are:

1. **Auth Protocol**: `test_gmail.go` uses `smtp.PlainAuth`, but Listmonk's current DB config uses `login`. I will update the database to use `plain`.
2. **Envelope Sender**: `test_gmail.go` explicitly sets the envelope sender (`MAIL FROM`) to the authenticated user. I will enforce this in `email.go`.
3. **Header Logic**: `test_gmail.go` constructs a clean `From` header matching the authenticated user. I've already partially implemented this, but I will refine it to exactly match the test script's robust fallback logic (using "Listmonk" as a default name if needed).

**Implementation Plan:**

1. **Update Database**: Set SMTP auth protocol to `plain`.
2. **Refactor** **`email.go`**:

   * Ensure the `Sender` (envelope sender) is *always* the authenticated username.

   * Refine the `From` header construction to mirror `test_gmail.go`'s logic: if the provided `From` doesn't match the authenticated user, rewrite it to use the authenticated address while preserving the name (or defaulting to "Listmonk").
3. **Verify**: Restart backend and trigger campaign.

