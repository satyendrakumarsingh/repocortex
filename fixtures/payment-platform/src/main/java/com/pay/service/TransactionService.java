package com.pay.service;

import com.pay.repository.TransactionRepository;
import com.pay.notification.NotificationService;

@Service
public class TransactionService {

    private final TransactionRepository repository;
    private final NotificationService notificationService;

    public TransactionService(TransactionRepository repository,
                              NotificationService notificationService) {
        this.repository = repository;
        this.notificationService = notificationService;
    }

    public void updateStatus(String transactionId, String status) {
        Transaction txn = repository.findById(transactionId);
        txn.setStatus(status);
        repository.save(txn);
        notificationService.send(transactionId, status);
    }

    public Transaction findTransaction(String transactionId) {
        return repository.findById(transactionId);
    }
}
