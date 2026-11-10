package com.acme.payments;

import jakarta.persistence.Column;
import jakarta.persistence.Entity;
import jakarta.persistence.Id;

/**
 * Payment declares no @Table, so the default naming applies. `Payment` has no
 * internal camel-case boundary, so Spring Boot's
 * CamelCaseToUnderscoresNamingStrategy and plain Hibernate's standard strategy
 * agree on `payment` once lowercased, and the mapping is reported certain.
 */
@Entity
public class Payment {

    @Id
    @Column(name = "payment_id")
    private Long id;

    @Column(name = "merchant_id")
    private String merchantId;

    private String status;

    public Long getId() {
        return id;
    }
}
