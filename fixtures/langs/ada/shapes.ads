with Ada.Text_IO;

package Shapes is

   Max_Sides : constant := 12;
   Default_Name : constant String := "shape";
   Counter : Natural := 0;
   Left, Right : Integer;

   type Color is (Red, Green, Blue);
   subtype Small is Integer range 1 .. 10;
   type Vector is array (1 .. 3) of Float;
   type Index is range 0 .. 100;
   type Handle is access Integer;

   type Point is record
      X : Float;
      Y : Float;
   end record;

   type Shape is tagged record
      Name : String (1 .. 8);
   end record;

   type Circle is new Shape with record
      Radius : Float;
   end record;

   type Printable is interface;
   procedure Print (Item : Printable) is abstract;

   type Opaque is private;
   type Secret is limited private;

   procedure Reset;
   procedure Move (P : in out Point; Dx, Dy : Float);
   function Area (S : Shape) return Float;
   function "+" (A, B : Point) return Point;
   function Is_Empty (S : Shape) return Boolean is (S.Name = "");

   procedure Long_Signature
     (First  : Integer;
      Second : Integer;
      Third  : Integer);

   package Nested is
      Depth : constant Integer := 3;
      procedure Descend;
   end Nested;

   package Alias renames Ada.Text_IO;

   generic
      type Element is private;
   package Stacks is
      type Stack is private;
      procedure Push (S : in out Stack; E : Element);
   private
      type Stack is record
         Top : Natural;
      end record;
   end Stacks;

   generic
      type T is private;
   procedure Swap (A, B : in out T);

   procedure Alias_Proc renames Reset;

   package Int_Stacks is new Stacks (Integer);
   procedure Swap_Int is new Swap (Integer);
   function Fresh return Float;
   type Meters is delta 0.01 range 0.0 .. 1000.0;
   type Mask is mod 256;
   type Real is digits 8;
   type Callback is access procedure (Code : Integer);
   type Shape_Ref is access all Shape'Class;

private

   type Opaque is new Integer;
   type Secret is record
      Key : Integer;
   end record;

end Shapes;
