package com.pay.controller;

import com.pay.service.TransactionService;

/**
 * Receives payment callbacks from the upstream provider.
 *
 * Known gap: concurrent callbacks for the same transaction are not currently
 * de-duplicated, which is the subject of the golden task fixture.
 */
@RestController
@RequestMapping("/payment")
public class CallbackController {

    private final TransactionService transactionService;

    public CallbackController(TransactionService transactionService) {
        this.transactionService = transactionService;
    }

    @PostMapping("/callback")
    public ResponseEntity<Void> handleCallback(@RequestBody CallbackRequest request) {
        transactionService.updateStatus(request.getTransactionId(), request.getStatus());
        return ResponseEntity.ok().build();
    }
}
