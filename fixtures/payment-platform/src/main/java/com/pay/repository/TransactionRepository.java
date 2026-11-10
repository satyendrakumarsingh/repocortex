package com.pay.repository;

@Repository
public class TransactionRepository {

    private final JdbcTemplate jdbc;

    public TransactionRepository(JdbcTemplate jdbc) {
        this.jdbc = jdbc;
    }

    public Transaction findById(String transactionId) {
        return jdbc.queryForObject(
            "SELECT id, merchant_id, status, amount FROM transactions WHERE id = ?",
            new TransactionMapper(), transactionId);
    }

    public void save(Transaction txn) {
        jdbc.update(
            "UPDATE transactions SET status = ?, updated_at = now() WHERE id = ?",
            txn.getStatus(), txn.getId());
        jdbc.update(
            "INSERT INTO transaction_events (transaction_id, event_type) VALUES (?, ?)",
            txn.getId(), "STATUS_CHANGED");
    }
}
