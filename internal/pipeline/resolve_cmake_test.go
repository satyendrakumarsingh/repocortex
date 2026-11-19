package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestCMakeResolution indexes a small project (a top-level file, helper files it
// includes, a find module, two subdirectories and a vendored namesake) and checks
// every command call against what `include`, `find_package` and `add_subdirectory`
// make it reach.
//
// What it is built to catch: `shop_log`, which cmake/helpers.cmake and the vendored
// third_party/cmake/helpers.cmake both define, where the file that includes
// `cmake/helpers.cmake` means the first; `include(CompilerWarnings)`, a module found
// by its name; an include through `${CMAKE_CURRENT_LIST_DIR}`; `include(local.cmake)`
// beside a subdirectory's file; a command of a file the top level runs and a
// subdirectory calls (low: which file ran first is not read); and the program's own
// commands (`set`, `add_library`, `message`, `target_link_libraries`), which a
// definition of the repository does not replace.
//
// Command names are case-insensitive: `Shop_Add_Module(...)` runs `shop_add_module`.
//
// What it does not bind, by design: a command that two files define (shop_log called from
// a file that includes neither), and a file that an included file includes.
func TestCMakeResolution(t *testing.T) {
	got := langEdges(t, cmakeFixture())
	high, low := model.ConfidenceHigh, model.ConfidenceLow
	wants := []want{
		{"shop/CMakeLists.txt:(top level)", "shop/CMakeLists.txt:shop_summary", high},
		{"shop/CMakeLists.txt:(top level)", "shop/cmake/CompilerWarnings.cmake:shop_enable_warnings", high},
		{"shop/CMakeLists.txt:shop_summary", "shop/cmake/helpers.cmake:shop_log", high},
		{"shop/cmake/helpers.cmake:shop_add_module", "shop/cmake/helpers.cmake:shop_log", high},
		{"shop/src/CMakeLists.txt:(top level)", "shop/src/local.cmake:src_tweak", high},
		// Defined in a file the top level runs; called below it.
		{"shop/src/CMakeLists.txt:(top level)", "shop/cmake/helpers.cmake:shop_add_module", low},
		{"shop/src/CMakeLists.txt:(top level)", "shop/cmake/packaging.cmake:shop_package", low},
		{"shop/src/CMakeLists.txt:(top level)", "shop/cmake/FindFmt.cmake:fmt_report", low},
		{"shop/src/plugins/CMakeLists.txt:(top level)", "shop/cmake/helpers.cmake:shop_add_module", low},
		{"shop/src/plugins/CMakeLists.txt:(top level)", "shop/cmake/packaging.cmake:shop_package", low},
		{"shop/src/plugins/CMakeLists.txt:(top level)", "shop/src/local.cmake:src_tweak", low},
		{"shop/tests/CMakeLists.txt:(top level)", "shop/cmake/CompilerWarnings.cmake:shop_enable_warnings", low},
		{"shop/tests/CMakeLists.txt:(top level)", "shop/cmake/helpers.cmake:shop_add_module", low},
		// A command written in other case than it is defined: shop_only_caps has
		// two definitions, and the included file's is the one meant.
		{"shop/CMakeLists.txt:shop_summary", "shop/cmake/helpers.cmake:Shop_Only_Caps", high},
	}
	forbid := []want{
		{Src: "", Dst: "third_party/"},
		// Two files define it, and neither of these includes either.
		{Src: "shop/src/local.cmake", Dst: ":shop_log"},
	}
	scoreEdges(t, got, wants, forbid, nil)
	// Only what a file states it runs, or the one definition there is, is more than a guess.
	capped(t, got, []want{
		{Src: "shop/src/", Dst: "shop/cmake/", Min: low},
		{Src: "shop/tests/", Dst: "shop/cmake/", Min: low},
	})
}

func cmakeFixture() map[string]string {
	return map[string]string{
		"shop/CMakeLists.txt": `cmake_minimum_required(VERSION 3.20)
project(shop LANGUAGES CXX)

list(APPEND CMAKE_MODULE_PATH "${CMAKE_SOURCE_DIR}/cmake")
include(cmake/helpers.cmake)
include(CompilerWarnings)
include("${CMAKE_CURRENT_LIST_DIR}/cmake/packaging.cmake")
find_package(Fmt REQUIRED)

function(shop_summary)
  message(STATUS "shop")
  shop_log("configured")
  SHOP_ONLY_CAPS()
endfunction()

add_subdirectory(src)
add_subdirectory(tests)
shop_summary()
shop_enable_warnings(shop_core)
`,
		"shop/cmake/helpers.cmake": `function(shop_log msg)
  message(STATUS "[shop] ${msg}")
endfunction()

function(Shop_Only_Caps)
endfunction()

macro(shop_add_module name)
  add_library(${name} ${ARGN})
  shop_log("module ${name}")
  set_target_properties(${name} PROPERTIES FOLDER shop)
endmacro()
`,
		"shop/cmake/CompilerWarnings.cmake": `function(shop_enable_warnings target)
  target_compile_options(${target} PRIVATE -Wall)
  shop_log("warnings for ${target}")
endfunction()
`,
		"shop/cmake/packaging.cmake": `function(shop_package name)
  install(TARGETS ${name})
endfunction()
`,
		"shop/cmake/FindFmt.cmake": `function(fmt_report)
  message(STATUS "fmt")
endfunction()
`,
		"third_party/cmake/helpers.cmake": `function(shop_only_caps)
endfunction()

function(shop_log msg)
  message("[vendor] ${msg}")
endfunction()
`,
		"shop/src/CMakeLists.txt": `include(local.cmake)
shop_add_module(shop_core core.cpp)
shop_package(shop_core)
src_tweak(shop_core)
add_subdirectory(plugins)
fmt_report()
`,
		"shop/src/local.cmake": `function(src_tweak target)
  shop_log("tweaking ${target}")
  target_compile_definitions(${target} PRIVATE SHOP=1)
endfunction()
`,
		"shop/src/plugins/CMakeLists.txt": `shop_add_module(shop_plugin plugin.cpp)
src_tweak(shop_plugin)
shop_package(shop_plugin)
Shop_Add_Module(shop_other other.cpp)
`,
		"shop/tests/CMakeLists.txt": `enable_testing()
add_executable(shop_tests main.cpp)
shop_enable_warnings(shop_tests)
shop_add_module(shop_test_util util.cpp)
`,
	}
}
