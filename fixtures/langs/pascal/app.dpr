program App;

{$mode objfpc}

uses Shapes;

type
  TApp = object
    Value: Integer;
    procedure Run;
  end;

var
  Main: TApp;

procedure TApp.Run;
begin
  WriteLn(Value);
end;

function Twice(N: Integer): Integer;
begin
  Result := N * 2;
end;

begin
  Main.Run;
  WriteLn(Twice(2));
end.
