package com.acme.payments;

import java.time.Instant;
import java.util.List;

import org.springframework.data.jpa.repository.JpaRepository;

public interface PaymentRepository extends JpaRepository<Payment, Long> {

    List<Payment> findByMerchantId(String merchantId);

    long countByStatus(String status);

    void deleteByCreatedAtBefore(Instant cutoff);
}
