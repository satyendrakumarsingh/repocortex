package com.acme.payments;

import jakarta.persistence.EntityManager;
import jakarta.persistence.PersistenceContext;

public class PaymentDao {

    @PersistenceContext
    private EntityManager entityManager;

    public Payment load(Long id) {
        return entityManager.find(Payment.class, id);
    }

    public void store(Payment payment) {
        entityManager.persist(payment);
    }
}
