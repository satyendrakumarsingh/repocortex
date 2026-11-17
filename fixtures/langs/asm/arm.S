    .syntax unified
    .global entry
    .thumb_func
entry:
    push {r4, lr}
    bl   compute
    pop  {r4, pc}

compute:
    movs r0, #1
.Lloop:
    subs r0, r0, #1
    bne  .Lloop
    bx   lr
