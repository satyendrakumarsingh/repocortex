package defs_pkg;
    parameter int DATA_W = 32;
    localparam int ADDR_W = 16;
    typedef enum logic [1:0] { A, B } state_t;
    function automatic int twice(int v);
        return v * 2;
    endfunction
endpackage

interface bus_if #(parameter W = 8) (input logic clk);
    logic [W-1:0] data;
    modport master (output data);
endinterface

class Packet;
    int len;
    function new();
        len = 0;
    endfunction
    function int size();
        return len;
    endfunction
    task send();
    endtask
endclass

module top (input logic clk);
    import defs_pkg::*;
    localparam LIMIT = 10;
    bus_if bus (.clk(clk));
    alu #(.WIDTH(4)) a0 (.a(), .b(), .y());
endmodule
