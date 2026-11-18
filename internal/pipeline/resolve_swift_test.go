package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestSwiftResolution indexes a SwiftPM package of four targets and checks
// every call against what the language says it reaches.
//
// What it is built to catch: a name defined in two modules where only one is
// imported (Money and formatMoney in Core and Billing), a standard-library
// function that shares its name with a repository one (max), a framework call
// that shares a name with a function of the same module (post), a method of
// a type nothing states (save on two classes), and a module reached with no
// import because it is the caller's own (Order in Core/Models calling
// Money.zero in Core).
func TestSwiftResolution(t *testing.T) {
	got := langEdges(t, swiftFixture())
	high := model.ConfidenceHigh
	wants := []want{
		// An import names the target; files below it are in it.
		{"App/Cart.swift:Cart.checkout", "Core/Money.swift:Money", high},
		{"App/Cart.swift:Cart.checkout", "Core/Money.swift:formatMoney", high},
		{"App/Cart.swift:Cart.checkout", "Core/Models/Order.swift:Order.total", high},
		{"App/Cart.swift:Cart.order", "Core/Models/Order.swift:Order", high},
		{"App/Cart.swift:Cart.persist", "Core/Cache.swift:makeCache", high},
		{"App/Cart.swift:Cart.store", "Core/Store.swift:Store", high},
		{"Core/Cache.swift:makeCache", "Core/Cache.swift:Cache", high},
		// The module's own files need no import, in this directory or a
		// directory below it.
		{"App/Cart.swift:Cart.checkout", "App/Router.swift:post", high},
		{"Core/Models/Order.swift:Order.total", "Core/Money.swift:Money.zero", high},
		{"Core/Models/Order.swift:Order.total", "Core/Money.swift:formatMoney", high},
		{"Core/Models/Order.swift:Order.total", "Core/Models/Order.swift:Order.log", high},
		{"Core/Money.swift:Money.zero", "Core/Money.swift:Money", high},
		// Billing resolves to Billing, which it is, and not to Core.
		{"Billing/Invoice.swift:render", "Billing/Money.swift:formatMoney", high},
		// A declared type states the receiver.
		{"App/Cart.swift:Cart.persist", "Core/Store.swift:Store.save", high},
		// Two classes declare save, and `thing` states no type, but makeCache
		// declares that it returns a Cache (returned.go).
		{"App/Cart.swift:Cart.persist", "Core/Cache.swift:Cache.save", high},
		// The test target imports the module under test.
		{"CartTests.swift:CartTests.testCheckout", "App/Cart.swift:Cart", high},
		{"CartTests.swift:CartTests.testCheckout", "App/Cart.swift:Cart.checkout", high},
		{"CartTests.swift:CartTests.testCheckout", "App/Router.swift:track", high},
		{"App/Router.swift:post", "App/Router.swift:track", high},
		// A call on a value of unstated type: `t = order.total()` and
		// `sum = Money.zero()`, each assigned from a function declared to
		// return Money, so the receiver is a Money (returned.go). Before return
		// types were read these were bound at low, by the name alone.
		{"Core/Models/Order.swift:Order.total", "Core/Money.swift:Money.adding", high},
		{"App/Cart.swift:Cart.checkout", "Core/Money.swift:Money.adding", high},
	}
	forbid := []want{
		// Billing is not imported by App.
		{Src: "App/Cart.swift", Dst: "Billing/"},
		{Src: "Tests/", Dst: "Billing/"},
		// And Core is not imported by Billing.
		{Src: "Billing/Invoice.swift", Dst: "Core/"},
		// max is the standard library's.
		{Src: "App/Cart.swift:Cart.checkout", Dst: "Billing/Invoice.swift:max"},
	}
	extraOK := []want{
		{"Core/Money.swift:Money.adding", "Core/Money.swift:Money", high},
	}
	scoreEdges(t, got, wants, forbid, extraOK)
}

func swiftFixture() map[string]string {
	return map[string]string{
		"Package.swift": `// swift-tools-version:5.9
import PackageDescription
let package = Package(name: "Shop", targets: [
    .target(name: "Core"),
    .target(name: "Billing"),
    .target(name: "App", dependencies: ["Core"]),
    .testTarget(name: "AppTests", dependencies: ["App"]),
])
`,
		"Sources/Core/Money.swift": `import Foundation

public struct Money {
    public let cents: Int
    public init(cents: Int) { self.cents = cents }
    public static func zero() -> Money { return Money(cents: 0) }
    public func adding(_ other: Money) -> Money { return Money(cents: cents + other.cents) }
}

public func formatMoney(_ m: Money) -> String {
    return "\(m.cents)"
}
`,
		"Sources/Core/Models/Order.swift": `import Foundation

public class Order {
    var lines: [Money] = []
    public init() {}
    public func total() -> Money {
        var sum = Money.zero()
        for l in lines { sum = sum.adding(l) }
        log(formatMoney(sum))
        return sum
    }
    func log(_ s: String) { print(s) }
}
`,
		"Sources/Core/Store.swift": `public class Store {
    public init() {}
    public func save(_ o: Order) {}
}
`,
		"Sources/Core/Cache.swift": `public class Cache {
    public init() {}
    public func save(_ o: Order) {}
    public func clear() {}
}

public func makeCache() -> Cache { return Cache() }
`,
		"Sources/Billing/Invoice.swift": `public func render(_ m: Money) -> String {
    return formatMoney(m)
}

public func max(_ a: Int, _ b: Int) -> Int { return a > b ? a : b }
`,
		"Sources/Billing/Money.swift": `public struct Money {
    public let amount: Double
    public init(amount: Double) { self.amount = amount }
}

public func formatMoney(_ m: Money) -> String {
    return "\(m.amount)"
}
`,
		"Sources/App/Cart.swift": `import Foundation
import UIKit
import Core

public class Cart {
    let order = Order()
    func checkout(url: URL) -> String {
        let t = order.total()
        let fee = Money(cents: 30)
        post(url)
        NotificationCenter.default.post(name: n, object: url)
        let biggest = max(1, 2)
        return formatMoney(t.adding(fee)) + String(biggest)
    }

    let store: Store = Store()

    func persist(o: Order) {
        store.save(o)
        let thing = makeCache()
        thing.save(o)
    }
}
`,
		"Sources/App/Router.swift": `import Foundation

func post(_ url: URL) {
    track(url)
}

func track(_ url: URL) { }
`,
		"Tests/AppTests/CartTests.swift": `import XCTest
@testable import App

final class CartTests: XCTestCase {
    func testCheckout() {
        let c = Cart()
        _ = c.checkout(url: URL(string: "x")!)
        track(URL(string: "y")!)
    }
}
`,
	}
}
