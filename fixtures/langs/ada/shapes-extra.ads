package Shapes.Extra is

   Pi : constant Float := 3.14159;

   procedure Spin (S : in out Shape);

   type Marker is null record;

   protected Registry is
      procedure Register (Id : Integer);
      function Count return Natural;
   private
      Total : Natural := 0;
   end Registry;

   task Ticker is
      entry Tick;
   end Ticker;

end Shapes.Extra;
