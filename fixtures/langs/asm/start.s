    .text
    .globl _start
    .globl main
    .type main, @function

_start:
    call main
    mov $60, %rax
    syscall

main:
    push %rbp
    mov %rsp, %rbp
    call helper
    jmp .Ldone
.Ldone:
    pop %rbp
    ret
LBB0_1:
    nop
    .size main, .-main

helper:
1:
    dec %rcx
    jnz 1b
    ret

    .type  util, @function
util:
    ret
