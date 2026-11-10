package com.pay.notification;

@Service
public class NotificationService {

    private final ExternalNotificationClient client;

    public NotificationService(ExternalNotificationClient client) {
        this.client = client;
    }

    public void send(String transactionId, String status) {
        client.post(transactionId, status);
    }
}
