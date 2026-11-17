unit Shapes;

interface

uses SysUtils, Classes;

const
  MaxShapes = 100;
  Pi2 = 6.28318;

type
  TColour = (Red, Green, Blue);
  TName = string;
  TPoint = record
    X, Y: Integer;
  end;
  PPoint = ^TPoint;

  IDrawable = interface
    procedure Draw;
    function Area: Double;
  end;

  TShape = class(TObject)
  private
    FName: string;
  public
    constructor Create(const AName: string);
    destructor Destroy; override;
    procedure Draw; virtual;
    function Area: Double; virtual; abstract;
    property Name: string read FName;
  end;

  TCircle = class(TShape)
    FRadius: Double;
    function Area: Double; override;
  end;

var
  Count: Integer;
  Names: array[0..9] of string;

procedure Reset;
function Total(A, B: Integer): Integer;

implementation

const
  Hidden = 7;

var
  Cache: Integer;

constructor TShape.Create(const AName: string);
begin
  inherited Create;
  FName := AName;
end;

destructor TShape.Destroy;
begin
  inherited;
end;

procedure TShape.Draw;
var
  Local: Integer;
begin
  Local := 1;
  WriteLn(Local);
end;

function TCircle.Area: Double;
begin
  Result := Pi2 * FRadius * FRadius;
end;

procedure Reset;
  procedure Inner;
  begin
  end;
begin
  Inner;
  Count := 0;
end;

function Total(A, B: Integer): Integer;
begin
  Result := A + B;
end;

end.
