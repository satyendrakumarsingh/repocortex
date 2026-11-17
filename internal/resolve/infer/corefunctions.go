package infer

import "strings"

// # Bare names a language's own library provides
//
// corenames.go lists the MEMBER names a language's core types declare, so that
// `x.toString()` is not bound to the one `toString` a repository declares. This
// is the same argument for a call written with no receiver: `require(x)` in
// Kotlin, `fgets(buf, n, f)` in C, `include Mod` in a Ruby class body and
// `format!(...)` in Rust name something the language or its standard library
// provides, and a repository that happens to declare ONE function of that name
// somewhere says nothing about which a call reaches. The other definitions are
// in a library that was never indexed.
//
// The principle is the one the member lists state: the language's standard
// library and its dominant test framework, nothing wider and not tuned to any
// project. What a name here costs is the correct binding of a repository
// function that shares it, called from a file that neither defines it nor
// imports it. A definition in the caller's own file, or in the type it is
// written in, or one the file imports by name, is evidence about THIS call, and
// is never refused on account of a list. Only the rules that pick a target by
// its name alone are (see bareByName and bareBeyondType).

// coreFunctions maps a language to the bare names its library provides.
var coreFunctions = buildCoreFunctions()

// coreSyntax maps a language to names that are invoked with a bare call and
// can never be a repository's function: Rust's standard macros. The parser
// records `format!(x)` as a call to `format` with no receiver, and a macro
// namespace is separate from the function one, so a function that shares the
// name is not what the invocation reaches even in the same file. Measured on
// fd, where `format!` was bound at `high` to a test function named `format`.
//
// Kept to names whose use as a function is the exception. `write`, `print`,
// `matches`, `file`, `line` and the like are macros too, and are in
// coreFunctions instead: refused to a name-only rule, allowed to a definition
// the file itself holds.
var coreSyntax = map[string]map[string]struct{}{
	"rust": words(`
		println eprintln eprint format format_args vec panic assert assert_eq assert_ne
		debug_assert debug_assert_eq debug_assert_ne todo unimplemented unreachable
		writeln dbg`),
}

func buildCoreFunctions() map[string]map[string]struct{} {
	out := make(map[string]map[string]struct{}, 8)

	// Rust: the macros that are also plausible function names.
	out["rust"] = words(`
		write print matches file line column env option_env concat stringify include
		include_str include_bytes cfg module_path compile_error thread_local`)

	// Kotlin: kotlin.* and kotlin.collections.*, which every file imports
	// implicitly, and the constructors of the JVM types a Kotlin file names
	// without an import. Measured on moshi, where `require(...)` and `Pair(...)`
	// were bound to a private member of an unrelated class and to a Java test
	// class.
	out["kotlin"] = words(`
		require check error assert requireNotNull checkNotNull TODO
		listOf mutableListOf arrayListOf setOf mutableSetOf hashSetOf linkedSetOf
		mapOf mutableMapOf hashMapOf linkedMapOf sortedMapOf emptyList emptySet emptyMap
		emptySequence sequenceOf arrayOf arrayOfNulls intArrayOf longArrayOf byteArrayOf
		charArrayOf booleanArrayOf doubleArrayOf floatArrayOf shortArrayOf
		lazy lazyOf println print readLine readln repeat run with apply let also
		buildString buildList buildMap buildSet sequence generateSequence synchronized
		runCatching maxOf minOf Pair Triple Regex Result
		String StringBuilder Array List Map Set Any Unit Exception RuntimeException
		IllegalArgumentException IllegalStateException UnsupportedOperationException
		NoSuchElementException IndexOutOfBoundsException AssertionError Error Throwable
		ArrayList HashMap HashSet LinkedHashMap LinkedHashSet`)
	out["scala"] = words(`
		require assert assume println print Some None Left Right Option Seq List Map Set
		Vector Array String StringBuilder Try Success Failure Future Promise Tuple2
		Tuple3 Ordering`)

	// Ruby: Kernel and Module, called with no receiver in a class body or a
	// method. Measured on rack, where `include BadRequest` was bound 18 times to
	// the one `include` the repository declares (Rack::Recursive#include).
	out["ruby"] = words(`
		include extend prepend require require_relative load attr_reader attr_writer
		attr_accessor private public protected module_function alias_method
		raise puts print p pp format sprintf printf loop lambda proc catch throw
		block_given? binding freeze at_exit gets sleep rand srand exit abort warn fail
		open system exec fork trap Integer Float String Array Hash Rational Complex`)

	// Swift: the standard library's free functions. A call of one with no
	// receiver is the library's unless the file or an import names a definition,
	// and a repository's own `max` or `print` helper in some other module says
	// nothing about it.
	out["swift"] = words(`
		print debugPrint dump assert assertionFailure precondition preconditionFailure
		fatalError min max abs zip stride swap readLine repeatElement numericCast
		withUnsafePointer withUnsafeMutablePointer withExtendedLifetime type`)

	// Lua: the base library, which is global and which a module of the
	// repository cannot hide from a file that does not require it.
	out["lua"] = words(`
		print require pairs ipairs tostring tonumber type assert error pcall xpcall
		select setmetatable getmetatable rawget rawset rawequal rawlen next unpack
		load loadstring loadfile dofile collectgarbage`)

	// Bash: the shell's own builtins and the utilities a script runs by name.
	// A command word that names one of them is that command, even when some
	// other script of the repository defines a function of the same name; a
	// function the file itself defines, or one it sources, is never refused (see
	// the note at the top of this file). Names a script plausibly defines for
	// itself (`install`, `build`, `check`, `run`, `usage`, `die`) are not here.
	out["bash"] = words(`
		alias bg bind break builtin caller cd command compgen complete compopt continue
		declare dirs disown echo enable eval exec exit export false fc fg getopts hash
		help history jobs kill let local logout mapfile popd printf pushd pwd read
		readarray readonly return set shift shopt source suspend test times trap true
		type typeset ulimit umask unalias unset wait
		awk basename cat chgrp chmod chown cp curl cut date dd df diff dirname du env
		find git grep gzip head hostname id jq ln ls md5sum mkdir mktemp mv nc ps rm
		rmdir rsync scp sed seq sha256sum sleep sort ssh stat tail tar tee touch tr
		uname uniq wc which whoami xargs docker kubectl terraform npm pip pip3 python
		python3 node go cargo java ruby perl sudo tput`)

	// R: base, stats, utils and methods, which are attached to every session, and
	// testthat's expectations, which every test file calls. A name here is the
	// language's whatever a script of the repository defines under it, from a
	// file this one does not define or `source()`.
	out["r"] = words(`
		c list vector numeric character logical integer double complex matrix array factor
		data.frame list2env environment new.env local eval evalq parse deparse substitute
		quote bquote body formals args arity match.arg match.call match.fun missing on.exit
		do.call Recall Reduce Filter Map Position Find lapply sapply vapply mapply rapply
		tapply apply outer sweep aggregate by split unsplit tryCatch try stop warning message
		signalCondition simpleError simpleCondition conditionMessage stopifnot invisible
		return print cat format formatC sprintf paste paste0 toString nchar substr substring
		strsplit gsub sub grepl grep regexpr gregexpr regmatches toupper tolower trimws
		startsWith endsWith sprintf rev sort order unique duplicated which which.max which.min
		any all is.na is.null is.numeric is.character is.function is.list is.logical ifelse
		length names setNames nrow ncol NROW NCOL dim rownames colnames levels nlevels table
		seq seq_len seq_along rep head tail append sum mean median min max range var sd
		round floor ceiling abs sqrt exp log log2 log10 cumsum cumprod prod rnorm runif sample
		set.seed quantile cor lm glm predict t.test filter select rbind cbind merge subset
		with within transform setdiff union intersect identical all.equal isTRUE isFALSE
		exists get get0 mget assign rm library require requireNamespace loadNamespace
		suppressWarnings suppressMessages suppressPackageStartupMessages source sys.source
		sys.call sys.function Sys.time Sys.Date Sys.getenv Sys.setenv Sys.sleep file.path
		file.exists dir.create list.files readLines writeLines readRDS saveRDS read.csv
		write.csv read.table write.table basename dirname normalizePath nlevels unlist
		structure class inherits attr attributes oldClass unclass is as new validObject
		setClass setGeneric setMethod setValidity setRefClass isVirtualClass standardGeneric
		callNextMethod UseMethod NextMethod R6Class slot slotNames str summary mapply
		Negate Vectorize nargs interactive commandArgs quit q date format.Date as.Date
		is.element match nchar rev max min pmin pmax cut findInterval
		test_that describe it context skip skip_if skip_if_not skip_on_cran expect_equal
		expect_identical expect_true expect_false expect_null expect_error expect_warning
		expect_message expect_silent expect_length expect_match expect_s3_class
		expect_s4_class expect_type expect_named expect_setequal expect_gt expect_lt
		expect_gte expect_lte expect_output expect_condition expect_no_error
		expect_snapshot expect_is expect_that expect_invisible`)

	// Elixir: Kernel, which every module imports, and ExUnit's macros, which
	// every test calls. A bare call of one is the language's unless the module
	// it is written in defines it or an `import` names it (elixir.go).
	out["elixir"] = words(`
		abs apply binding ceil div elem exit floor function_exported? get_and_update_in
		get_in hd inspect is_atom is_binary is_bitstring is_boolean is_float is_function
		is_integer is_list is_map is_nil is_number is_pid is_port is_reference is_struct
		is_tuple is_exception length make_ref map_size max min node not pop_in put_elem
		put_in raise reraise rem round self send spawn spawn_link spawn_monitor struct
		struct! tl throw trunc tuple_size update_in to_string to_charlist then tap dbg
		byte_size bit_size binary_part is_map_key
		test describe setup setup_all assert refute assert_receive refute_receive
		assert_raise assert_received refute_received flunk on_exit doctest start_supervised
		start_supervised! capture_io capture_log`)

	// PowerShell: the cmdlets and aliases of the modules every session loads, the
	// executables a script runs by name, and Pester's commands. Only a function
	// the repository defines binds, and a function that shares a name with one of
	// these is the cmdlet from any file that does not define it itself. Kept
	// lowercase: command names are case-insensitive.
	out["powershell"] = words(`
		add-content add-member add-type clear-content clear-host clear-item clear-variable compare-object
		convert-path convertfrom-json convertfrom-csv convertfrom-securestring convertto-json convertto-csv
		convertto-html convertto-securestring convertto-xml copy-item disable-psbreakpoint enable-psbreakpoint
		enter-pssession exit-pssession export-clixml export-csv export-modulemember foreach-object
		format-custom format-list format-table format-wide get-alias get-childitem get-command get-content
		get-credential get-date get-event get-eventlog get-help get-history get-host get-item get-itemproperty
		get-job get-location get-member get-module get-process get-psdrive get-pssession get-random
		get-service get-unique get-variable get-wmiobject get-ciminstance get-filehash get-acl get-culture
		get-executionpolicy get-hotfix get-netipaddress get-nettcpconnection get-psprovider get-timezone
		group-object import-clixml import-csv import-module import-localizeddata invoke-command invoke-expression
		invoke-item invoke-restmethod invoke-webrequest invoke-rest invoke-sqlcmd join-path measure-command
		measure-object move-item new-alias new-item new-itemproperty new-module new-object new-psdrive
		new-pssession new-service new-timespan new-variable out-default out-file out-host out-null out-string
		pop-location push-location read-host receive-job register-argumentcompleter register-objectevent
		remove-item remove-itemproperty remove-job remove-module remove-psdrive remove-pssession
		remove-variable rename-item resolve-path restart-service select-object select-string select-xml
		send-mailmessage set-acl set-alias set-content set-date set-executionpolicy set-item set-itemproperty
		set-location set-psdebug set-psreadlineoption set-service set-strictmode set-variable sort-object
		split-path start-job start-process start-service start-sleep start-transcript stop-process
		stop-service stop-transcript tee-object test-connection test-json test-path test-netconnection
		trace-command unblock-file update-help wait-job wait-process where-object write-debug write-error
		write-host write-information write-output write-progress write-verbose write-warning write-eventlog
		cd ls dir cat cp mv rm del echo cls pwd gci gc gcm gi gl gm gp gv iex irm iwr kill ps sls sort tee
		select where foreach measure sleep start saps sasv spps spsv md mkdir type man help
		git docker kubectl npm node python python3 pip dotnet nuget msbuild curl wget ssh scp az aws gh terraform
		describe context it beforeall afterall beforeeach aftereach should mock assert-mockcalled
		invoke-pester new-pesterconfiguration inmodulescope skip set-itresult`)

	// Erlang: the functions of the `erlang` module the compiler imports into every
	// module (auto-imported BIFs). A module cannot define one of its own under the
	// same name and arity without `-compile({no_auto_import, ...})`, so a bare call
	// of one is the language's. A definition in the same file is never refused
	// (see the note at the top of this file); an `-import`, which the compiler
	// also rejects for these names, does not override them either.
	out["erlang"] = words(`
		abs apply atom_to_binary atom_to_list binary_part binary_to_atom binary_to_existing_atom
		binary_to_float binary_to_integer binary_to_list binary_to_term bit_size bitstring_to_list
		byte_size ceil date demonitor disconnect_node element erase error exit float
		float_to_binary float_to_list floor garbage_collect get get_keys group_leader halt hd
		integer_to_binary integer_to_list iolist_size iolist_to_binary is_alive is_atom is_binary
		is_bitstring is_boolean is_float is_function is_integer is_list is_map is_map_key is_number
		is_pid is_port is_process_alive is_record is_reference is_tuple length link list_to_atom
		list_to_binary list_to_bitstring list_to_existing_atom list_to_float list_to_integer
		list_to_pid list_to_tuple make_ref map_get map_size max min module_loaded monitor
		monitor_node node nodes now open_port pid_to_list port_close port_command process_flag
		process_info processes put register registered round self setelement size spawn
		spawn_link spawn_monitor spawn_opt split_binary statistics term_to_binary throw time tl
		trunc tuple_size tuple_to_list unlink unregister whereis`)

	// Clojure: clojure.core, which every namespace refers, and clojure.test's
	// forms. A bare call of one is the language's unless the file defines it or
	// a `:refer` names it (modules.go).
	out["clojure"] = words(`
		* *' + +' - -' / < <= = == > >= abs agent apply assoc assoc-in associative? atom bigdec bigint
		binding boolean boolean? butlast byte char char? class cond-> conj conj! cons constantly contains?
		count cycle dec dedupe deref dissoc dissoc! distinct distinct? doall dorun drop drop-last drop-while
		empty empty? even? every-pred every? false? ffirst filter filterv find first flatten float fn? fnext
		for format frequencies get get-in group-by hash-map hash-set identical? identity inc int int? interleave
		interpose into into-array iterate juxt keep keep-indexed key keys keyword keyword? last lazy-cat
		list list? long map map-indexed map? mapcat mapv max max-key merge merge-with meta min min-key mod
		name namespace neg? next nfirst nil? nnext not not-any? not-empty not-every? not= nth nthnext nthrest
		number? odd? partial partition partition-all partition-by peek persistent! pop pop! pos? pr pr-str
		print print-str println prn quot rand rand-int rand-nth range re-find re-matches re-pattern re-seq
		reduce reduce-kv reductions rem remove repeat repeatedly replace rest reverse reversible? rseq
		second select-keys seq seq? sequence sequential? set set? short shuffle some some? sort sort-by
		sorted-map sorted-set split-at split-with str string? subs subvec swap! symbol symbol? take
		take-last take-nth take-while time transduce transient true? type unchecked-add update update-in
		val vals vec vector vector? volatile! vreset! vswap! when-first with-meta zero? zipmap
		ex-info ex-data ex-message resolve requiring-resolve gensym eval load-file
		run! mapv pmap pcalls future-call promise deliver realized? force
		testing is are deftest run-tests run-all-tests use-fixtures thrown? thrown-with-msg?`)

	// Julia: Base, which every module imports, and Core. A bare call of one is
	// the language's unless the file defines it (a method of a function that
	// extends Base is its own definition) or `using` names it (modules.go).
	out["julia"] = words(`
		abs abs2 all any append! argmax argmin axes bitstring broadcast cat ceil chomp chop
		close cld collect contains copy copyto! count cumsum cumprod deepcopy delete! deleteat!
		diff display div divrem eachindex eachline eachsplit eltype empty empty! endswith enumerate
		eof error eval exit exp extrema fill fill! filter filter! findall findfirst findlast findmax
		findmin first fld flatten floor foldl foldr get get! getindex getproperty haskey hash hcat
		identity ifelse in include insert! intersect isa isapprox isdigit isempty isequal iseven
		isfile isless ismissing isnothing isodd isone isopen isreal isspace issubset isuppercase
		iszero iterate join joinpath keys keytype last lastindex length lock log log10 log2 lowercase
		lpad lstrip map map! mapfoldl mapreduce match max maximum merge merge! min minimum mod
		mkpath modf ndims nextind ntuple occursin one open parent parse parse! permute! pop!
		popfirst! position print println printstyled prod push! pushfirst! put! rand randn read
		readdir readline readlines readuntil reduce rem repeat replace replace! reshape resize!
		rethrow reverse reverse! round rpad rsplit rstrip searchsortedfirst setdiff setindex!
		setproperty! show similar sizehint! sizeof size sleep sort sort! split splice! sprint
		sqrt startswith string strip sum symdiff take! throw time tryparse typeof typemax typemin
		union unique unique! unsafe_load uppercase values vcat vec view wait warn write zero zeros
		ones fieldnames getfield setfield! hasfield nameof isdefined fieldtype supertype isconcretetype
		convert promote promote_type Dict Vector Matrix Array Set String Int Int8 Int16 Int32 Int64
		UInt UInt8 UInt16 UInt32 UInt64 Float16 Float32 Float64 Bool Char Symbol Tuple NamedTuple
		Pair Ref Ptr Some Missing Nothing Union Type Any Number Real Integer AbstractString
		ArgumentError BoundsError DomainError ErrorException KeyError MethodError AssertionError
		DimensionMismatch Exception InterruptException OverflowError StackOverflowError TypeError
		ismutable invokelatest hasmethod methods which sum! prod! mean_ cmp isinteger length_`)

	// Dart: the dart:core functions a file can call with no import.
	out["dart"] = words(`print identical identityHashCode`)

	// C and C++: the C library, which a repository may wrap or interpose (jq's
	// fault-injection `fgets`) and a test calls through to.
	libc := words(`
		fgets fputs fopen fclose fread fwrite fprintf printf sprintf snprintf vsnprintf
		sscanf scanf fscanf puts putchar getc getchar fgetc fputc fflush fseek ftell
		rewind feof ferror perror setvbuf malloc calloc realloc free memcpy memmove
		memset memcmp memchr strlen strcmp strncmp strcpy strncpy strcat strncat strchr
		strrchr strstr strtok strdup strerror strtol strtoul strtod strtoll atoi atol
		atof abs labs qsort bsearch exit abort atexit getenv setenv unsetenv putenv
		system rand srand time clock difftime mktime gmtime localtime strftime sleep
		usleep nanosleep open close read write lseek stat fstat lstat isatty dup dup2
		pipe fork wait waitpid kill signal raise getpid unlink rename mkdir rmdir
		chdir getcwd opendir readdir closedir mmap munmap select poll socket bind
		listen accept connect send recv sin cos tan sqrt pow floor ceil fabs fmod log
		exp isalpha isdigit isspace isupper islower toupper tolower assert
		pthread_mutex_lock pthread_mutex_unlock pthread_mutex_init pthread_mutex_destroy
		pthread_once pthread_key_create pthread_key_delete pthread_getspecific
		pthread_setspecific pthread_create pthread_join
		clearerr freopen putc ungetc fgetpos fsetpos setbuf tmpfile vfprintf vprintf vsprintf
		vfscanf vscanf vsscanf`)
	out["c"] = libc
	out["cpp"] = libc
	// Objective-C includes the C library, and Foundation adds NSLog.
	objc := words("NSLog")
	for name := range libc {
		objc[name] = struct{}{}
	}
	out["objc"] = objc

	// ECMAScript: the platform's own constructors and namespaces, which a bare
	// `new URL(...)` or `Response(...)` names whether or not the repository
	// declares a class of the same name elsewhere.
	script := make(map[string]struct{}, len(scriptBuiltins))
	for _, b := range scriptBuiltins {
		script[b] = struct{}{}
	}
	for _, lang := range []string{"javascript", "typescript", "tsx", "vue", "svelte", "astro"} {
		out[lang] = script
	}
	return out
}

// isCoreFunction reports whether the language's own library provides a
// callable of this bare name.
func (s LanguageSpec) isCoreFunction(name string) bool {
	if s.Name == "powershell" {
		// Command names are case-insensitive, and the list is kept lowercase.
		name = strings.ToLower(name)
	}
	_, ok := coreFunctions[s.Name][name]
	return ok
}

// isCoreSyntax reports whether a bare call of this name is the language's own
// syntax, so that nothing the repository defines is what it reaches.
func (s LanguageSpec) isCoreSyntax(name string) bool {
	_, ok := coreSyntax[s.Name][name]
	return ok
}
