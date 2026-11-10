package com.pay.model;

public class Transaction {
    private String id;
    private String merchantId;
    private String status;
    private long amount;

    public String getId() { return id; }
    public String getStatus() { return status; }
    public void setStatus(String status) { this.status = status; }
    public String getMerchantId() { return merchantId; }
}
