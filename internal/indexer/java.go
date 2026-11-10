package indexer

// javaArgs builds the scip-java argv for one project.
//
// scip-java does not analyse source the way scip-go and scip-typescript do. It
// runs the repository's OWN Maven or Gradle build with an instrumented javac,
// and then reads the SemanticDB files that compilation emitted. Everything
// below exists because that makes indexing inherit every constraint the build
// has, and three real repositories each failed for a different one:
//
//   - spring-boot: Gradle only, and Gradle was not installed. Nothing to do
//     here; the planner reports the missing tool.
//   - spring-petclinic: ships BOTH pom.xml and build.gradle, so scip-java
//     refused to guess and exited with "Multiple build tools detected".
//   - commons-lang: built successfully and emitted no SemanticDB at all,
//     because the javac plugin does not attach at -source 8; raising it then
//     hit the javadoc plugin, then checkstyle.
//
// The first is not fixable from here. The second and third are.
func javaArgs(p Project, indexPath string) []string {
	args := []string{"index", "--output", indexPath}

	if tool := javaBuildTool(p); tool != "" {
		// Chosen from the marker FILES, never from their contents: a build
		// script is attacker-controlled text and this value reaches an argv.
		args = append(args, "--build-tool="+tool)
	}

	if extra := javaBuildArgs(p); len(extra) > 0 {
		// Everything after `--` goes to the build tool rather than to
		// scip-java.
		args = append(args, "--")
		args = append(args, extra...)
	}
	return args
}

// javaBuildTool decides which build to drive when a repository ships more than
// one set of build files.
//
// Maven wins a tie deliberately. A repository carrying both usually carries
// Gradle for the developers and Maven for publishing, the Maven build is the
// more self-contained of the two, and Gradle additionally wants a daemon and a
// wrapper download that a container may not have. Where only one marker is
// present the choice is not a preference at all.
func javaBuildTool(p Project) string {
	maven := p.hasMarker("pom.xml")
	gradle := p.hasMarker("build.gradle") || p.hasMarker("build.gradle.kts") ||
		p.hasMarker("settings.gradle") || p.hasMarker("settings.gradle.kts")

	switch {
	case maven:
		return "maven"
	case gradle:
		return "gradle"
	default:
		// scip-java also understands Bazel and sbt, which RepoCortex does not
		// detect. Saying nothing lets scip-java decide for itself rather than
		// being told something wrong.
		return ""
	}
}

// javaBuildArgs are the flags handed to the build tool.
//
// They turn off work that cannot affect the index and can fail the build. The
// index needs one thing from the build: compilation, with the SemanticDB plugin
// attached. Tests, javadoc, checkstyle, spotbugs, PMD, license audits and
// enforcer rules produce nothing this reads, and each of them independently
// stopped a real repository from being indexed — commons-lang failed first on
// javadoc warnings and then on 49 checkstyle violations, neither of which says
// anything about whether its code can be resolved.
//
// This is not "make the build pass regardless". A compilation error still fails
// the run, because without compilation there is nothing to index.
func javaBuildArgs(p Project) []string {
	switch javaBuildTool(p) {
	case "maven":
		return []string{
			// The goals come FIRST and are not optional. Passing anything after
			// `--` REPLACES scip-java's own goal list, so a build given only
			// properties runs with no lifecycle phase at all and Maven exits
			// with "No goals have been specified for this build". That failure
			// hides well: commons-lang survived it because its pom declares a
			// defaultGoal, and spring-petclinic did not.
			"clean", "verify",
			"-DskipTests",
			"-Dmaven.test.skip=true",
			"-Dmaven.javadoc.skip=true",
			"-Dcheckstyle.skip=true",
			"-Dspotbugs.skip=true",
			"-Dpmd.skip=true",
			"-Dcpd.skip=true",
			"-Drat.skip=true",
			"-Dlicense.skip=true",
			"-Denforcer.skip=true",
			"-Danimal.sniffer.skip=true",
			"-Djacoco.skip=true",
			// Properties only past this point, never a flag: scip-java adds
			// its own --batch-mode, and a second one broke its parsing.
		}
	case "gradle":
		// Same shape: the task comes first, then the exclusions.
		return []string{"build", "-x", "test", "-x", "javadoc", "-x", "checkstyleMain"}
	default:
		return nil
	}
}
