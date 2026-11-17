with Shapes;

procedure Demo is
   P : Shapes.Point := (X => 1.0, Y => 2.0);
   Total : Integer := 0;

   function Twice (N : Integer) return Integer is
   begin
      return N * 2;
   end Twice;

   generic
      type Item is private;
   procedure Show (X : Item);

   procedure Show (X : Item) is
   begin
      null;
   end Show;
begin
   Shapes.Reset;
   Total := Twice (3);
end Demo;
