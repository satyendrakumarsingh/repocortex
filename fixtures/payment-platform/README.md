# payment-platform (test fixture)

A deliberately small Spring Boot-shaped service used as RepoCortex's golden
fixture. Referenced by spec §42 (fixture repositories) and §43 (golden test).

## Expected task-context result

For the task **"Add idempotency to payment callback processing"**,
`get_task_context` must surface:

- `CallbackController`      — the entry point
- `TransactionService`      — where idempotency logic belongs
- `TransactionRepository`   — persistence, writes `transactions.status`
- `NotificationService`     — the duplicate side effect being prevented
- `transactions`            — table written on status change
- `transaction_events`      — audit rows written per change
- `CallbackControllerTest`  — existing regression coverage
- `TransactionServiceTest`  — existing regression coverage

and must EXCLUDE `unrelated/marketing/**`, which shares no dependency path with
the callback flow. That exclusion is the real test: retrieving the right files
is easy when you return everything.
