package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestGroovyResolution indexes a small Gradle project, a Spock specification and
// a JUnit test, and checks every call against what the JVM's rules make it reach.
//
// What it is built to catch: `Money.zero()` where two packages declare a `Money`
// and only one is imported; `import a.b.*`, `import a.b.C as D` and a class of
// the file's own package, which needs no import; `new TaxCalc()` as a construction;
// `this.helper(x)` and a bare call inside a closure; `JsonOutput.toJson` and
// `println`, which leave the repository; and a build script, whose `test { }`,
// `dependencies { }` and `apply` are Gradle's and not the `test` and `apply`
// methods that classes of the project declare. (A build.gradle is a manifest to the
// indexer and is not read as Groovy source, so it has no references to bind; a
// Jenkinsfile is read, and its steps are not calls of the project's functions.)
func TestGroovyResolution(t *testing.T) {
	got := langEdges(t, groovyFixture())
	high := model.ConfidenceHigh
	wants := []want{
		// An import by class name, and a package neighbour with no import.
		{"shop/Cart.groovy:Cart.total", "money/Money.groovy:Money.zero", high},
		{"shop/Cart.groovy:Cart.total", "shop/Inventory.groovy:Inventory.count", high},
		// `m` holds what Money.zero() returned, which the source does not state: the
		// one `plus` of the repository, at low.
		{"shop/Cart.groovy:Cart.total", "money/Money.groovy:Money.plus", model.ConfidenceLow},
		// A wildcard import, and an alias.
		{"shop/Cart.groovy:Cart.total", "tax/Rounding.groovy:Rounding.round", high},
		{"shop/Cart.groovy:Cart.total", "fmt/Formatter.groovy:Formatter.format", high},
		// A construction, and a call on what it constructed.
		{"shop/Cart.groovy:Cart.total", "tax/TaxCalc.groovy:TaxCalc", high},
		{"shop/Cart.groovy:Cart.total", "tax/TaxCalc.groovy:TaxCalc.rate", high},
		// A static import of a method.
		{"shop/Cart.groovy:Cart.total", "util/Strings.groovy:Strings.slug", high},
		// Members of the class itself, through `this` and bare, closures included.
		{"shop/Cart.groovy:Cart.total", "shop/Cart.groovy:Cart.helper", high},
		{"shop/Cart.groovy:Cart.checkout", "shop/Cart.groovy:Cart.total", high},
		// A Spock feature method, named by its title, and a JUnit test.
		{"shop/CartSpec.groovy:totals an empty cart", "shop/Cart.groovy:Cart", high},
		{"shop/CartSpec.groovy:totals an empty cart", "shop/Cart.groovy:Cart.total", high},
		{"shop/CartTest.groovy:CartTest.totals", "shop/Cart.groovy:Cart", high},
		{"shop/CartTest.groovy:CartTest.totals", "shop/Cart.groovy:Cart.total", high},
		{"shop/CartTest.groovy:CartTest.totals", "money/Money.groovy:Money.zero", high},
	}
	forbid := []want{
		// other.Money is not the Money the file imports.
		{Src: "shop/Cart.groovy", Dst: "other/Money.groovy"},
		{Src: "shop/CartTest.groovy", Dst: "other/Money.groovy"},
		// A build script's `test`, `apply` and `deploy` are Gradle's.
		{Src: "build.gradle", Dst: "tasks/Task.groovy"},
		{Src: "Jenkinsfile", Dst: "vars/deploy.groovy"},
		{Src: "Jenkinsfile", Dst: "tasks/Task.groovy"},
		{Src: "settings.gradle", Dst: "tasks/Task.groovy"},
		// util.Strings.pad is not statically imported.
		{Src: "shop/Cart.groovy", Dst: "util/Strings.groovy:Strings.pad"},
	}
	// Each class constructs itself in its own factory, and a script's function calls
	// another of its own.
	extraOK := []want{
		{"vars/deploy.groovy:deploy", "vars/deploy.groovy:call", high},
		{"money/Money.groovy:Money.zero", "money/Money.groovy:Money", high},
		{"other/Money.groovy:Money.zero", "other/Money.groovy:Money", high},
	}
	// `stats` is what an undefined `loadStats()` returned, and `ship` is declared by
	// one type of the repository: the name alone says nothing about a value whose
	// type is not stated, and the edge is low. It is wrong, and is counted.
	known := []want{{"shop/Cart.groovy:Cart.total", "shop/Warehouse.groovy:Warehouse.ship", model.ConfidenceLow}}
	scoreEdges(t, got, wants, forbid, extraOK, known...)
}

func groovyFixture() map[string]string {
	const main = "src/main/groovy/com/acme/"
	const test = "src/test/groovy/com/acme/"
	return map[string]string{
		"settings.gradle": `rootProject.name = 'shop'
include 'app'
`,
		"build.gradle": `plugins {
    id 'groovy'
}

dependencies {
    implementation 'org.codehaus.groovy:groovy:3.0.9'
    testImplementation 'org.spockframework:spock-core:2.0-groovy-3.0'
}

def versionOf(name) {
    return "1.0-" + name
}

test {
    useJUnitPlatform()
}

apply plugin: 'java'

tasks.register('hello') {
    doLast {
        println versionOf('x')
    }
}
`,
		// A pipeline library's function, which the Jenkinsfile calls by `deploy` as a
		// step of the pipeline DSL and not as a function of this repository.
		"Jenkinsfile": `pipeline {
    agent any
    stages {
        stage('Deploy') {
            steps {
                deploy('x')
                test('y')
            }
        }
    }
}
`,
		"vars/deploy.groovy": `def call(name) {
    println name
}

def deploy(name) {
    call(name)
}
`,
		main + "money/Money.groovy": `package com.acme.money

class Money {
    static Money zero() { new Money() }

    Money plus(Money other) { this }
}
`,
		main + "other/Money.groovy": `package com.acme.other

class Money {
    static Money zero() { new Money() }
}
`,
		main + "tax/TaxCalc.groovy": `package com.acme.tax

class TaxCalc {
    def rate(m) { 0.2 }
}
`,
		main + "tax/Rounding.groovy": `package com.acme.tax

class Rounding {
    static round(x) { x }
}
`,
		main + "fmt/Formatter.groovy": `package com.acme.fmt

class Formatter {
    static String format(m) { "" + m }
}
`,
		main + "util/Strings.groovy": `package com.acme.util

class Strings {
    static slug(s) { s }

    static pad(s) { s }
}
`,
		// A class with the names Gradle's DSL uses.
		main + "tasks/Task.groovy": `package com.acme.tasks

class Task {
    def test(x) { x }

    def apply(x) { x }

    def dependencies(x) { x }
}
`,
		main + "shop/Warehouse.groovy": `package com.acme.shop

class Warehouse {
    def ship(order) { order }
}
`,
		main + "shop/Inventory.groovy": `package com.acme.shop

class Inventory {
    static count() { 0 }
}
`,
		main + "shop/Cart.groovy": `package com.acme.shop

import com.acme.money.Money
import com.acme.tax.*
import com.acme.fmt.Formatter as Fmt
import static com.acme.util.Strings.slug
import groovy.json.JsonOutput

class Cart {
    def total(List items) {
        def m = Money.zero()
        m.plus(m)
        Inventory.count()
        Rounding.round(m)
        Fmt.format(m)
        def t = new TaxCalc()
        t.rate(m)
        slug("x")
        def stats = loadStats()
        stats.ship()
        this.helper(items)
        items.each { helper(it) }
        JsonOutput.toJson(items)
        println "total"
        return m
    }

    def checkout(c) {
        total(c)
    }

    private helper(x) { x }
}
`,
		test + "shop/CartSpec.groovy": `package com.acme.shop

import spock.lang.Specification

class CartSpec extends Specification {
    def "totals an empty cart"() {
        given:
        def cart = new Cart()

        expect:
        cart.total([]) != null
    }
}
`,
		test + "shop/CartTest.groovy": `package com.acme.shop

import com.acme.money.Money
import org.junit.Test

class CartTest {
    @Test
    void totals() {
        def cart = new Cart()
        cart.total([])
        Money.zero()
    }
}
`,
	}
}
