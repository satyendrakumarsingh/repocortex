package com.pay.controller;

public class CallbackControllerTest {

    @Test
    public void handlesCallbackSuccessfully() {
        controller.handleCallback(request);
        verify(transactionService).updateStatus("txn-1", "COMPLETED");
    }

    @Test
    public void returnsOkOnValidPayload() {
        assertEquals(200, controller.handleCallback(request).getStatusCodeValue());
    }
}
