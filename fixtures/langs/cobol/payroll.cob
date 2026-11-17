       IDENTIFICATION DIVISION.
       PROGRAM-ID. PAYROLL.
       AUTHOR. NOBODY.

       ENVIRONMENT DIVISION.
       INPUT-OUTPUT SECTION.
       FILE-CONTROL.
           SELECT EMP-FILE ASSIGN TO "EMP.DAT".

       DATA DIVISION.
       FILE SECTION.
       FD  EMP-FILE.
       01  EMP-RECORD.
           05  EMP-NAME        PIC X(20).
           05  EMP-SALARY      PIC 9(7)V99.

       WORKING-STORAGE SECTION.
       77  WS-COUNT            PIC 9(4) VALUE 0.
       01  WS-TOTAL            PIC 9(9)V99 VALUE 0.
       01  WS-FLAGS.
           05  WS-EOF          PIC X VALUE "N".
               88  AT-END      VALUE "Y".
           05  WS-ERR          PIC X.

       LINKAGE SECTION.
       01  LS-PARM             PIC X(10).

       PROCEDURE DIVISION USING LS-PARM.
       MAIN-PARA.
           PERFORM INIT-PARA.
           PERFORM READ-LOOP UNTIL AT-END.
           PERFORM REPORT-SECTION.
           STOP RUN.

       INIT-PARA.
           MOVE 0 TO WS-TOTAL.
           OPEN INPUT EMP-FILE.

       READ-LOOP.
           READ EMP-FILE
               AT END MOVE "Y" TO WS-EOF
           END-READ.
           ADD EMP-SALARY TO WS-TOTAL.

       REPORT-SECTION SECTION.
       PRINT-PARA.
           DISPLAY "TOTAL " WS-TOTAL.
           CALL "AUDITLOG" USING WS-TOTAL.

       END PROGRAM PAYROLL.
