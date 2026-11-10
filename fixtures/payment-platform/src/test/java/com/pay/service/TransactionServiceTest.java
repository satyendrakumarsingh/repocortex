package com.pay.service;

public class TransactionServiceTest {

    @Test
    public void updatesStatusAndNotifies() {
        service.updateStatus("txn-1", "COMPLETED");
        verify(repository).save(any());
        verify(notificationService).send("txn-1", "COMPLETED");
    }

    @Test
    public void findsTransactionById() {
        assertNotNull(service.findTransaction("txn-1"));
    }
}
