`timescale 1ns / 1ps

module alu #(
    parameter WIDTH = 8,
    parameter DEPTH = 4
) (
    input  wire [WIDTH-1:0] a,
    input  wire [WIDTH-1:0] b,
    output reg  [WIDTH-1:0] y
);
    localparam IDLE = 0, BUSY = 1;
    parameter MODE = 2;

    reg [3:0] state;
    wire carry;

    function [WIDTH-1:0] add;
        input [WIDTH-1:0] x;
        input [WIDTH-1:0] z;
        begin
            add = x + z;
        end
    endfunction

    function automatic integer clog2(input integer v);
        integer i;
        begin
            clog2 = 0;
            for (i = v; i > 0; i = i >> 1) clog2 = clog2 + 1;
        end
    endfunction

    task reset_all;
        begin
            state = IDLE;
        end
    endtask

    task automatic bump(input integer n);
        begin
            state = state + n;
        end
    endtask

    always @(*) begin
        y = add(a, b);
        reset_all;
    end

    sub_unit #(.W(WIDTH)) u0 (.clk(a[0]), .q(carry));
endmodule

module sub_unit #(parameter W = 1) (input clk, output q);
    assign q = clk;
endmodule
