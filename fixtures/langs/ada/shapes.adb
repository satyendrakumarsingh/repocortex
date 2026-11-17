with Ada.Text_IO; use Ada.Text_IO;

package body Shapes is

   Hidden_Count : Natural := 0;
   Scale : constant Float := 2.0;

   procedure Reset is
      Local_Total : Integer := 0;
      Local_Limit : constant Integer := 4;
      procedure Helper is
      begin
         Put_Line ("helper");
      end Helper;
   begin
      Counter := 0;
      Helper;
      Print_Banner;
   end Reset;

   procedure Move (P : in out Point; Dx, Dy : Float) is
   begin
      P.X := P.X + Dx;
      P.Y := P.Y + Dy;
   end Move;

   function Area (S : Shape) return Float is
      function Square (V : Float) return Float is (V * V);
   begin
      return Square (Scale);
   end Area;

   function "+" (A, B : Point) return Point is
   begin
      return (X => A.X + B.X, Y => A.Y + B.Y);
   end "+";

   procedure Print (Item : Printable) is
   begin
      null;
   end Print;

   procedure Forward_Only;

   procedure Forward_Only is
   begin
      null;
   end Forward_Only;

   package body Nested is
      Inner_Count : Integer := 0;
      procedure Descend is
      begin
         Inner_Count := Inner_Count + 1;
      end Descend;
   end Nested;

   package body Stacks is
      procedure Push (S : in out Stack; E : Element) is
      begin
         S.Top := S.Top + 1;
      end Push;
   end Stacks;

   procedure Swap (A, B : in out T) is
      Tmp : T := A;
   begin
      A := B;
      B := Tmp;
   end Swap;

   task type Worker is
      entry Start;
   end Worker;

   task body Worker is
   begin
      accept Start;
   end Worker;

   protected type Lock is
      procedure Acquire;
   end Lock;

   protected body Lock is
      procedure Acquire is
      begin
         null;
      end Acquire;
   end Lock;

end Shapes;
