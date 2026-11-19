package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestFortranResolution indexes a small program (modules, an external procedure
// file, a main program) and checks every call against what `use` makes it reach.
//
// What it is built to catch: `area` in geometry and in geometry_legacy, where only
// the module a file `use`s is meant; `log_scale`, which geometry keeps private and
// geometry_legacy exports, so that a program that uses geometry does not reach
// either; a module named in capitals (`USE GEOMETRY, ONLY: perimeter`); `use m,
// only: name`, which brings in the one name and not its neighbours; an internal
// procedure (after the `contains` of a subroutine and of the program), which only
// its host and the host's other internal procedures call; an external procedure
// (`finish`, in a file of no module), called from anywhere; `max`, `size` and
// `mpi_barrier`, which are the language's and a library's; an array element
// `cs(i)`, which reads as a call; and a type-bound call `c%grow(2.0)`, which is a
// call on a value.
//
// Procedure names are case-insensitive: `CALL SCALE_SHAPE(...)` is `Scale_Shape`
// and `PERIMETER(c)` is `perimeter`, and the module the program uses decides
// which of two spellings (geometry_legacy has a `normalize_it` of its own) is meant.
//
// What it does not bind, by design: a generic interface name
// (`interface area`), a type-bound procedure, a name renamed by `use m, loc =>
// name`, and a procedure that an `include` line pulls in. An external procedure is
// bound at low: nothing in the call says the file is the one meant.
func TestFortranResolution(t *testing.T) {
	got := langEdges(t, fortranFixture())
	high, low := model.ConfidenceHigh, model.ConfidenceLow
	wants := []want{
		// A module's own procedures, public and private.
		{"src/geometry.f90:geometry.Scale_Shape", "src/geometry.f90:geometry.<private>.circle_grow", high},
		{"src/geometry.f90:geometry.Scale_Shape", "src/geometry.f90:geometry.<private>.log_scale", high},
		// `use stats, only: total_area` and `use geometry` in the program.
		{"app/main.f90:main", "src/stats.f90:stats.total_area", high},
		{"app/main.f90:main", "src/geometry.f90:geometry.Scale_Shape", high},
		{"app/main.f90:main", "src/geometry.f90:geometry.area", high},
		{"app/main.f90:main", "src/geometry.f90:geometry.perimeter", high},
		// An internal procedure of the program, and of a subroutine.
		{"app/main.f90:main", "app/main.f90:main.tidy", high},
		{"src/stats.f90:stats.summarize", "src/stats.f90:stats.summarize.<locals>.helper", high},
		// `use geometry, only: area` in one module, `use geometry_legacy, only: report` in another.
		{"src/stats.f90:stats.total_area", "src/geometry.f90:geometry.area", high},
		{"src/stats.f90:stats.total_area", "src/legacy/geometry.f90:geometry_legacy.report", high},
		// A procedure written in other case than it is defined.
		{"app/main.f90:main", "src/geometry.f90:geometry.Normalize_It", high},
		{"app/main.f90:main", "src/geometry.f90:geometry.scale_up", high},
		// A module spelled in capitals.
		{"src/report.f90:Reporting.show_perimeter", "src/geometry.f90:geometry.perimeter", high},
		// External procedures: the file of no module, bound by name.
		{"src/util.f90:finish", "src/util.f90:flush_it", high},
		{"app/main.f90:main", "src/util.f90:finish", low},
		{"app/main.f90:main.tidy", "src/util.f90:finish", low},
		{"src/stats.f90:stats.summarize", "src/util.f90:finish", low},
		{"src/stats.f90:stats.summarize.<locals>.helper", "src/util.f90:finish", low},
	}
	forbid := []want{
		// geometry_legacy is not the module the program uses.
		{Src: "app/main.f90", Dst: "src/legacy/geometry.f90"},
		{Src: "src/stats.f90:stats.total_area", Dst: "src/legacy/geometry.f90:geometry_legacy.area"},
		// The other spelling's module is not the one the program uses.
		{Src: "app/main.f90", Dst: "geometry_legacy.normalize_it"},
		{Src: "app/main.f90", Dst: "geometry_legacy.scale_up"},
		// A private procedure is not reached through `use`.
		{Src: "app/main.f90", Dst: "geometry.<private>"},
		{Src: "src/report.f90", Dst: "geometry.<private>"},
		// Not the internal procedure of another host, and not a type-bound procedure.
		{Src: "app/main.f90", Dst: "summarize.<locals>.helper"},
		{Src: "src/report.f90", Dst: "circle_grow"},
	}
	scoreEdges(t, got, wants, forbid, nil)
	capped(t, got, []want{{Src: "", Dst: "src/util.f90:finish", Min: low}})
}

func fortranFixture() map[string]string {
	return map[string]string{
		"src/geometry.f90": `module geometry
  implicit none
  private
  public :: area, perimeter, Scale_Shape, Normalize_It, scale_up
  type, public :: circle
    real :: r
  contains
    procedure :: grow => circle_grow
  end type circle
contains
  function area(c) result(a)
    type(circle), intent(in) :: c
    real :: a
    a = 3.14 * c%r**2
  end function area

  function perimeter(c) result(p)
    type(circle), intent(in) :: c
    real :: p
    p = 2.0 * 3.14 * c%r
  end function perimeter

  subroutine circle_grow(self, k)
    class(circle), intent(inout) :: self
    real, intent(in) :: k
    self%r = self%r * k
  end subroutine circle_grow

  subroutine Normalize_It(k)
    real, intent(in) :: k
  end subroutine Normalize_It

  subroutine scale_up(k)
    real, intent(in) :: k
  end subroutine scale_up

  subroutine Scale_Shape(c, k)
    type(circle), intent(inout) :: c
    real, intent(in) :: k
    call circle_grow(c, k)
    call log_scale(k)
  end subroutine Scale_Shape

  subroutine log_scale(k)
    real, intent(in) :: k
    print *, k
  end subroutine log_scale
end module geometry
`,
		"src/legacy/geometry.f90": `module geometry_legacy
  implicit none
contains
  function area(r) result(a)
    real :: r, a
    a = r * r
  end function area

  subroutine log_scale(k)
    real, intent(in) :: k
  end subroutine log_scale

  subroutine report(k)
    real, intent(in) :: k
  end subroutine report

  subroutine normalize_it(k)
    real, intent(in) :: k
  end subroutine normalize_it

  subroutine SCALE_UP(k)
    real, intent(in) :: k
  end subroutine SCALE_UP
end module geometry_legacy
`,
		"src/stats.f90": `module stats
  use geometry, only: area
  use geometry_legacy, only: report
  implicit none
contains
  function total_area(cs, n) result(t)
    use geometry, only: circle
    type(circle), intent(in) :: cs(n)
    integer, intent(in) :: n
    real :: t
    integer :: i
    t = 0.0
    do i = 1, n
      t = t + area(cs(i))
    end do
    call report(t)
    t = max(t, 0.0) + size(cs)
  end function total_area

  subroutine summarize(x)
    real, intent(in) :: x
    call helper(x)
    call finish(x)
  contains
    subroutine helper(y)
      real, intent(in) :: y
      call finish(y)
    end subroutine helper
  end subroutine summarize
end module stats
`,
		"src/util.f90": `subroutine finish(x)
  real, intent(in) :: x
  call flush_it(x)
end subroutine finish

subroutine flush_it(x)
  real, intent(in) :: x
end subroutine flush_it
`,
		"src/report.f90": `MODULE Reporting
  USE GEOMETRY, ONLY: perimeter
  IMPLICIT NONE
CONTAINS
  SUBROUTINE show_perimeter(c)
    USE geometry, ONLY: circle
    TYPE(circle) :: c
    PRINT *, perimeter(c)
    CALL c%grow(2.0)
  END SUBROUTINE show_perimeter
END MODULE Reporting
`,
		"app/main.f90": `program main
  use geometry
  use stats, only: total_area
  implicit none
  type(circle) :: cs(2)
  real :: t
  t = total_area(cs, 2)
  call Scale_Shape(cs(1), 2.0)
  CALL NORMALIZE_IT(2.0)
  CALL scale_UP(2.0)
  print *, perimeter(cs(1)), area(cs(2))
  call finish(t)
  call log_scale(t)
  call mpi_barrier(t)
  call tidy(t)
contains
  subroutine tidy(x)
    real :: x
    call finish(x)
  end subroutine tidy
end program main
`,
	}
}
