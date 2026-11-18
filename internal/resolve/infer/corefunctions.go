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
	// CUDA: the C library, and what the toolkit provides to device and host code
	// alike (the names with a prefix are in coreFunctionPrefixes).
	cuda := words(`
		atomicAdd atomicSub atomicExch atomicMin atomicMax atomicInc atomicDec atomicCAS
		atomicAnd atomicOr atomicXor rsqrtf sqrtf expf exp2f logf log2f log10f sinf cosf tanf
		powf fabsf fminf fmaxf floorf ceilf roundf truncf fmodf rintf sincosf erff tanhf
		min max abs dim3 printf assert malloc free memcpy memset`)
	for name := range libc {
		cuda[name] = struct{}{}
	}
	out["cuda"] = cuda
	// Objective-C includes the C library, and Foundation adds NSLog.
	objc := words("NSLog")
	for name := range libc {
		objc[name] = struct{}{}
	}
	out["objc"] = objc

	// Solidity: the global functions and the value-type conversions, which a
	// contract of the repository may share a name with (a `transfer` or an
	// `assert` of its own) and which a call without a receiver never reaches. A
	// conversion such as `uint256(x)` or `address(x)` is written as a call.
	out["solidity"] = words(`
		require assert revert keccak256 sha256 ripemd160 ecrecover addmod mulmod selfdestruct
		suicide blockhash gasleft blobhash
		address payable string bytes bool uint int byte
		uint8 uint16 uint24 uint32 uint40 uint48 uint56 uint64 uint72 uint80 uint88 uint96
		uint104 uint112 uint120 uint128 uint136 uint144 uint152 uint160 uint168 uint176 uint184
		uint192 uint200 uint208 uint216 uint224 uint232 uint240 uint248 uint256
		int8 int16 int24 int32 int40 int48 int56 int64 int72 int80 int88 int96 int104 int112
		int120 int128 int136 int144 int152 int160 int168 int176 int184 int192 int200 int208
		int216 int224 int232 int240 int248 int256
		bytes1 bytes2 bytes3 bytes4 bytes5 bytes6 bytes7 bytes8 bytes9 bytes10 bytes11 bytes12
		bytes13 bytes14 bytes15 bytes16 bytes17 bytes18 bytes19 bytes20 bytes21 bytes22 bytes23
		bytes24 bytes25 bytes26 bytes27 bytes28 bytes29 bytes30 bytes31 bytes32`)

	// Haskell: the Prelude, which every module imports, and the handful of Data.List,
	// Data.Maybe and Control.Monad names that code imports unqualified without a
	// list. A bare call of one is the language's unless the file defines it or an
	// import brings a repository definition of that name in (modules.go).
	out["haskell"] = words(`
		abs acos acosh all and any appendFile asTypeOf asin asinh atan atan2 atanh break ceiling compare
		concat concatMap const cos cosh curry cycle decodeFloat div divMod drop dropWhile either elem
		encodeFloat enumFrom enumFromThen enumFromThenTo enumFromTo error errorWithoutStackTrace even exp
		exponent fail filter flip floatDigits floor fmap foldMap foldl foldl1 foldr foldr1 fromEnum
		fromInteger fromIntegral fromRational fst gcd getChar getContents getLine head id init interact
		ioError isInfinite isNaN iterate last lcm length lex lines log logBase lookup map mapM mapM_
		mappend max maxBound maximum maybe mconcat mempty min minBound minimum mod negate not notElem
		null odd or otherwise pi pred print product properFraction pure putChar putStr putStrLn quot
		quotRem read readFile readIO readList readLn readParen reads readsPrec realToFrac recip rem
		repeat replicate return reverse round scaleFloat scanl scanl1 scanr scanr1 seq sequence
		sequenceA sequence_ show showChar showList showParen showString shows showsPrec significand
		signum sin sinh snd span splitAt sqrt subtract succ sum tail take takeWhile tan tanh toEnum
		toInteger toRational traverse truncate uncurry undefined unlines until unwords unzip unzip3
		userError words writeFile zip zip3 zipWith zipWith3
		catMaybes fromJust fromMaybe isJust isNothing listToMaybe mapMaybe maybeToList
		forM forM_ when unless void forever replicateM replicateM_ foldM zipWithM zipWithM_ guard
		liftA2 liftM liftM2 join mapAndUnzipM filterM mplus mzero ap
		sort sortBy sortOn nub nubBy group groupBy partition intercalate intersperse transpose
		isPrefixOf isSuffixOf isInfixOf foldl' find findIndex elemIndex insert delete union intersect
		tails inits zip4 unfoldr genericLength on comparing bool
		throwIO throw catch try evaluate bracket bracket_ finally handle
		newIORef readIORef writeIORef modifyIORef modifyIORef' atomicModifyIORef'
		hPutStrLn hPutStr hGetLine hClose hFlush withFile exitWith exitFailure exitSuccess getArgs getProgName
		describe it context specify shouldBe shouldSatisfy shouldReturn shouldThrow shouldNotBe
		property forAll testCase testGroup assertEqual assertBool assertFailure defaultMain
		hspec before after around beforeAll afterAll pending pendingWith`)

	// OCaml: the Stdlib module, which is opened in every file, and Stdlib's pervasive
	// operators written as names. A bare call of one is the language's unless the
	// file defines it or an `open` brings a repository definition of that name in.
	out["ocaml"] = words(`
		abs abs_float acos asin atan atan2 at_exit bool_of_string ceil char_of_int classify_float close_in
		close_out compare copysign cos cosh decr do_at_exit epsilon_float exit exp expm1 failwith float
		float_of_int float_of_string floor flush flush_all fmod frexp fst hypot ignore in_channel_length
		incr infinity input input_char input_line int_of_char int_of_float int_of_string invalid_arg ldexp
		lnot log log10 log1p max max_float max_int min min_float min_int mod_float modf nan not open_in
		open_out open_out_bin pred prerr_endline prerr_float prerr_int prerr_newline prerr_string print_char
		print_endline print_float print_int print_newline print_string raise raise_notrace read_float
		read_int read_line really_input ref seek_in set_binary_mode_in sin sinh snd sqrt string_of_bool
		string_of_float string_of_int succ tan tanh truncate`)

	// F#: FSharp.Core, whose operators and functions are opened in every file, and
	// the Expecto, xUnit and NUnit forms a test file calls bare. A bare call of one
	// is the language's unless the file defines it or an `open` brings a repository
	// definition of that name in.
	out["fsharp"] = words(`
		abs acos asin atan atan2 box byte ceil char compare cos cosh decimal defaultArg double enum exit
		exp failwith failwithf float float32 floor fst id ignore int int16 int32 int64 int8 invalidArg
		invalidOp isNull lock log log10 max min nameof not nullArg printf printfn raise reraise round
		seq sign sin sinh snd sqrt stderr stdin stdout string tan tanh truncate typeof typedefof uint16
		uint32 uint64 uint8 unbox ref sprintf eprintf eprintfn failwith async task lazy
		testCase testList testAsync testProperty`)

	// Perl: the functions the interpreter provides, which a bare call names whatever
	// a `use`d module also defines (the module's is `Module::name(...)`).
	out["perl"] = words(`
		abs accept alarm atan2 bind binmode bless caller chdir chmod chomp chop chown chr chroot close
		closedir connect cos crypt defined delete die do each eof eval exec exists exit exp fcntl fileno
		flock fork format getc getppid glob gmtime goto grep hex index int ioctl join keys kill last lc
		lcfirst length link listen local localtime log lstat map mkdir my next no oct open opendir ord our
		pack pipe pop pos print printf prototype push quotemeta rand read readdir readline readlink recv
		redo ref rename require reset return reverse rewinddir rindex rmdir say scalar seek seekdir select
		send shift sin sleep socket sort splice split sprintf sqrt srand stat study sub substr symlink
		syscall sysopen sysread sysseek system syswrite tell telldir tie tied time times truncate uc
		ucfirst umask undef unlink unpack unshift untie use utime values vec wait waitpid wantarray warn
		write`)

	// MATLAB: the functions every installation has, which a bare call names whatever
	// a file of the repository is called. A repository's own `zeros.m` would shadow
	// the builtin on the path, and nobody writes one; the rest of the language's
	// library is far larger than this list, and is what no repository file named.
	out["matlab"] = words(`
		zeros ones eye rand randn randi randperm magic linspace logspace repmat reshape size
		length numel ndims isempty isnumeric ischar iscell isstruct islogical isa isfield
		isreal isscalar isvector ismatrix isrow iscolumn isnan isinf isfinite class double
		single int8 int16 int32 int64 uint8 uint16 uint32 uint64 char logical cell struct
		fieldnames rmfield setfield getfield cellfun arrayfun structfun cell2mat num2cell
		num2str str2num str2double mat2str sprintf fprintf disp display error warning
		assert input pause tic toc clock cputime date datestr datenum now sum prod cumsum
		cumprod max min mean median std var mode sort sortrows unique find any all abs sqrt
		exp log log2 log10 sin cos tan asin acos atan atan2 sinh cosh tanh floor ceil round
		fix mod rem power times plus minus rdivide mtimes dot cross kron det inv pinv rank
		trace norm eig svd qr lu chol expm diag tril triu fliplr flipud flip rot90 circshift
		cat horzcat vertcat squeeze permute meshgrid ndgrid interp1 interp2 polyfit polyval
		roots conv deconv fft ifft plot plot3 figure hold axis xlabel ylabel zlabel title
		legend grid subplot surf mesh contour bar hist histogram scatter line text saveas
		close clf cla drawnow set get gca gcf colorbar colormap load save exist isfile
		isfolder mkdir rmdir delete copyfile movefile cd pwd ls dir fopen fclose fread fwrite
		fgetl fgets fscanf textscan readtable writetable csvread csvwrite dlmread dlmwrite
		importdata strcat strsplit strjoin strrep strfind strcmp strcmpi strncmp strncmpi
		strtrim upper lower regexp regexprep regexpi contains startsWith endsWith func2str
		str2func feval eval evalin assignin deal isequal ismember setdiff union intersect nnz
		diff accumarray bitand bitor bitxor bitshift mat2cell rethrow throw validateattributes
		nargin nargout narginchk nargoutchk exit quit clc clear clearvars format who whos which
		help doc type numel cummax cummin range sub2ind ind2sub colon isspace isletter
		isdigit fieldnames inputname nnz triu nan inf pi eps`)

	// Fortran: the intrinsic procedures, which a call names whatever a module of the
	// repository defines under the same name and does not bring in. The names are
	// lower case: the language does not tell cases apart (isCoreFunction).
	out["fortran"] = words(`
		abs achar acos acosh adjustl adjustr aimag aint all allocated anint any asin asinh
		associated atan atan2 atanh bit_size btest ceiling char cmplx command_argument_count
		conjg cos cosh count cpu_time cshift date_and_time dble digits dim dot_product dprod
		eoshift epsilon erf erfc exp exponent floor fraction gamma get_command
		get_command_argument get_environment_variable huge hypot iachar iand ibclr ibits ibset
		ichar ieor index int ior ishft ishftc kind lbound len len_trim lge lgt lle llt log
		log10 log_gamma logical matmul max maxloc maxval merge min minloc minval mod modulo
		move_alloc mvbits nearest new_line nint norm2 not null pack parity popcnt poppar
		precision present product radix random_number random_seed range real repeat reshape
		rrspacing scale scan selected_int_kind selected_real_kind set_exponent shape sign sin
		sinh size spacing spread sqrt storage_size sum system_clock tan tanh tiny transfer
		transpose trim ubound unpack verify c_loc c_f_pointer c_associated c_sizeof
		execute_command_line flush open close read write print rewind backspace inquire
		allocate deallocate nullify stop error stop`)

	// GLSL: the built-in functions and the constructors of the built-in types. A
	// shader file of the repository may define a `lerp` or a `saturate` of its own
	// for a target that lacks it, and a file that includes it (or defines it) is
	// still bound to it.
	out["glsl"] = words(`
		radians degrees sin cos tan asin acos atan sinh cosh tanh asinh acosh atanh pow exp
		log exp2 log2 sqrt inversesqrt abs sign floor trunc round roundEven ceil fract mod
		modf min max clamp mix step smoothstep isnan isinf floatBitsToInt floatBitsToUint
		intBitsToFloat uintBitsToFloat length distance dot cross normalize faceforward reflect
		refract matrixCompMult outerProduct transpose determinant inverse lessThan
		lessThanEqual greaterThan greaterThanEqual equal notEqual any all not texture
		textureLod textureProj textureOffset texelFetch textureSize textureGrad textureGather
		textureQueryLod textureQueryLevels texture1D texture2D texture3D textureCube
		texture2DLod imageLoad imageStore imageSize imageAtomicAdd atomicAdd atomicMin
		atomicMax atomicAnd atomicOr atomicXor atomicExchange atomicCompSwap dFdx dFdy fwidth
		dFdxFine dFdyFine dFdxCoarse dFdyCoarse fwidthFine fwidthCoarse interpolateAtCentroid
		interpolateAtSample interpolateAtOffset EmitVertex EndPrimitive EmitStreamVertex
		EndStreamPrimitive barrier memoryBarrier groupMemoryBarrier memoryBarrierShared
		memoryBarrierImage memoryBarrierBuffer packHalf2x16 unpackHalf2x16 packUnorm2x16
		unpackUnorm2x16 packUnorm4x8 unpackUnorm4x8 packSnorm4x8 unpackSnorm4x8 bitfieldExtract
		bitfieldInsert bitfieldReverse bitCount findLSB findMSB subpassLoad noise1 noise2
		noise3 noise4 float int uint bool double vec2 vec3 vec4 ivec2 ivec3 ivec4 uvec2 uvec3
		uvec4 bvec2 bvec3 bvec4 dvec2 dvec3 dvec4 mat2 mat3 mat4 mat2x2 mat2x3 mat2x4 mat3x2
		mat3x3 mat3x4 mat4x2 mat4x3 mat4x4 dmat2 dmat3 dmat4`)

	// HLSL: the intrinsic functions and the constructors of the scalar, vector and
	// matrix types.
	out["hlsl"] = words(`
		abs acos all any asin atan atan2 ceil clamp clip cos cosh cross ddx ddy ddx_coarse
		ddx_fine ddy_coarse ddy_fine degrees determinant distance dot exp exp2 faceforward
		floor fmod frac frexp fwidth isfinite isinf isnan ldexp length lerp lit log log10 log2
		mad max min modf mul normalize pow radians rcp reflect refract round rsqrt saturate
		sign sin sincos sinh smoothstep sqrt step tan tanh transpose trunc asfloat asint asuint
		asdouble f16tof32 f32tof16 countbits firstbitlow firstbithigh reversebits noise
		InterlockedAdd InterlockedMin InterlockedMax InterlockedAnd InterlockedOr InterlockedXor
		InterlockedExchange InterlockedCompareExchange InterlockedCompareStore
		GroupMemoryBarrier GroupMemoryBarrierWithGroupSync DeviceMemoryBarrier
		DeviceMemoryBarrierWithGroupSync AllMemoryBarrier AllMemoryBarrierWithGroupSync
		tex1D tex2D tex3D texCUBE tex2Dlod tex2Dbias WaveActiveSum WaveActiveMin WaveActiveMax
		WaveActiveAllTrue WaveActiveAnyTrue WaveActiveBallot WaveGetLaneIndex WaveGetLaneCount
		WaveIsFirstLane WaveReadLaneFirst WaveReadLaneAt WavePrefixSum QuadReadAcrossX
		QuadReadAcrossY QuadReadLaneAt GetRenderTargetSampleCount GetRenderTargetSamplePosition
		bool int uint half float double min16float min16int min16uint bool2 bool3 bool4 int2
		int3 int4 uint2 uint3 uint4 half2 half3 half4 float2 float3 float4 double2 double3
		double4 float2x2 float3x3 float4x4 float2x3 float2x4 float3x2 float3x4 float4x2 float4x3
		int2x2 int3x3 int4x4 uint2x2 uint3x3 uint4x4 half2x2 half3x3 half4x4 double2x2
		double3x3 double4x4 matrix vector`)

	// CMake: the commands the program itself provides, and those of the modules that
	// ship with it and that a project calls without having written them. The names
	// are lower case: command names are not case-sensitive (isCoreFunction).
	out["cmake"] = words(`
		add_compile_definitions add_compile_options add_custom_command add_custom_target
		add_definitions add_dependencies add_executable add_library add_link_options
		add_subdirectory add_test aux_source_directory break build_command cmake_file_api
		cmake_host_system_information cmake_language cmake_minimum_required cmake_parse_arguments
		cmake_path cmake_policy configure_file continue create_test_sourcelist define_property
		else elseif enable_language enable_testing endblock endforeach endfunction endif
		endmacro endwhile execute_process export file find_file find_library find_package
		find_path find_program fltk_wrap_ui foreach function get_cmake_property
		get_directory_property get_filename_component get_property get_source_file_property
		get_target_property get_test_property if include include_directories
		include_external_msproject include_guard include_regular_expression install
		link_directories link_libraries list load_cache load_command macro mark_as_advanced
		math message option project remove_definitions return separate_arguments set
		set_directory_properties set_property set_source_files_properties set_target_properties
		set_tests_properties site_name source_group string target_compile_definitions
		target_compile_features target_compile_options target_include_directories
		target_link_directories target_link_libraries target_link_options
		target_precompile_headers target_sources try_compile try_run unset variable_watch while
		block ctest_build ctest_configure ctest_coverage ctest_memcheck ctest_start ctest_submit
		ctest_test ctest_update ctest_upload qt_wrap_cpp qt_wrap_ui qt5_wrap_cpp qt5_wrap_ui
		check_include_file check_include_files check_include_file_cxx check_function_exists
		check_symbol_exists check_cxx_symbol_exists check_cxx_source_compiles
		check_c_source_compiles check_cxx_source_runs check_c_source_runs check_cxx_compiler_flag
		check_c_compiler_flag check_library_exists check_type_size check_struct_has_member
		check_language cmake_dependent_option cmake_print_variables cmake_print_properties
		cmake_push_check_state cmake_pop_check_state cmake_reset_check_state
		write_basic_package_version_file configure_package_config_file
		generate_export_header gtest_discover_tests gtest_add_tests fetchcontent_declare
		fetchcontent_makeavailable fetchcontent_populate fetchcontent_getproperties
		externalproject_add externalproject_add_step pkg_check_modules pkg_search_module
		find_package_handle_standard_args feature_summary add_feature_info set_package_properties
		include_directories_before cpack_add_component cpack_add_component_group
		cpack_add_install_type`)

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
	switch s.Name {
	case "powershell", "fortran", "cmake":
		// Command and procedure names are case-insensitive, and the list is kept lowercase.
		name = strings.ToLower(name)
	}
	if _, ok := coreFunctions[s.Name][name]; ok {
		return true
	}
	for _, prefix := range coreFunctionPrefixes[s.Name] {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// coreFunctionPrefixes are, per language, the beginnings of the names of a library's
// functions, for a library too large to list: the CUDA runtime and its math,
// random-number, FFT and neural-network libraries (`cudaMalloc`, `cublasSgemm`,
// `curand_uniform`), the compiler intrinsics that start with two underscores
// (`__syncthreads`, `__shfl_sync`), and the vector constructors (`make_float3`);
// and the message-passing and OpenMP runtimes a Fortran program calls.
var coreFunctionPrefixes = map[string][]string{
	"cuda":    {"cuda", "cublas", "cufft", "curand", "cusparse", "cusolver", "cudnn", "nvtx", "nvml", "nccl", "__", "make_float", "make_int", "make_uint", "make_double", "make_long", "make_short", "make_char", "make_uchar", "make_ulong", "make_ushort", "make_ulonglong", "make_longlong"},
	"fortran": {"mpi_", "omp_", "nf90_", "nf_", "h5", "blacs_", "pdgemm"},
}

// isCoreSyntax reports whether a bare call of this name is the language's own
// syntax, so that nothing the repository defines is what it reaches.
func (s LanguageSpec) isCoreSyntax(name string) bool {
	_, ok := coreSyntax[s.Name][name]
	return ok
}
