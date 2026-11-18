package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestApexResolution indexes a small Salesforce package and its tests and
// checks every call against the rule that an Apex class is global by name.
//
// What it is built to catch: `Money.zero()` and `new TaxCalc()` from a class in
// another directory with no import; an instance call on a variable of a stated
// type, where another class declares a method of the same name; `this.f()`,
// `super.f()` and a bare call to an inherited method; `System.debug`,
// `Database.insert`, `Test.startTest` and `Schema.SObjectType`, which are the
// platform's and not the `debug`, `insert` and `startTest` of a class of the
// org; an interface a class implements; and an @isTest class.
func TestApexResolution(t *testing.T) {
	got := langEdges(t, apexFixture())
	high := model.ConfidenceHigh
	const cls = "force-app/main/default/classes/"
	wants := []want{
		// A static call and a construction, by class name, from another directory.
		{cls + "svc/CartService.cls:CartService.checkout", cls + "util/Money.cls:Money.zero", high},
		{cls + "svc/CartService.cls:CartService.checkout", cls + "util/Money.cls:Money", high},
		{cls + "svc/CartService.cls:CartService.checkout", cls + "tax/TaxCalc.cls:TaxCalc", high},
		// An instance call on a variable of a stated type.
		{cls + "svc/CartService.cls:CartService.checkout", cls + "tax/TaxCalc.cls:TaxCalc.rate", high},
		{cls + "svc/CartService.cls:CartService.checkout", cls + "util/Money.cls:Money.plus", high},
		// `this.`, a bare call, and the one the superclass declares.
		{cls + "svc/CartService.cls:CartService.checkout", cls + "svc/CartService.cls:CartService.audit", high},
		{cls + "svc/CartService.cls:CartService.checkout", cls + "svc/BaseService.cls:BaseService.log", high},
		// `super.audit()` where the superclass and an implemented interface both
		// declare `audit`: the class's implementation is what runs, the
		// interface only declares it.
		{cls + "svc/CartService.cls:CartService.audit", cls + "svc/BaseService.cls:BaseService.audit", high},
		// A constructor chaining to super, and a class extending another.
		{cls + "svc/CartService.cls:CartService", cls + "svc/BaseService.cls:BaseService", high},
		{cls + "svc/CartService.cls:CartService", cls + "svc/IService.cls:IService", high},
		// A static call into a class that has a method named like the platform's.
		{cls + "svc/CartService.cls:CartService.checkout", cls + "util/Logger.cls:Logger.info", high},
		// Another class's static method, from a trigger-handler.
		{cls + "handlers/AccountHandler.cls:AccountHandler.onInsert", cls + "svc/CartService.cls:CartService.checkout", high},
		{"force-app/main/default/triggers/AccountTrigger.trigger:(top level)", cls + "handlers/AccountHandler.cls:AccountHandler.onInsert", high},
		{cls + "handlers/AccountHandler.cls:AccountHandler.onInsert", cls + "util/Money.cls:Money.zero", high},
		// An @isTest class.
		{cls + "svc/CartServiceTest.cls:CartServiceTest.testCheckout", cls + "svc/CartService.cls:CartService", high},
		{cls + "svc/CartServiceTest.cls:CartServiceTest.testCheckout", cls + "svc/CartService.cls:CartService.checkout", high},
		{cls + "svc/CartServiceTest.cls:CartServiceTest.testCheckout", cls + "util/Money.cls:Money.zero", high},
	}
	forbid := []want{
		{Src: cls + "ui/Page.cls", Dst: cls + "ui/Right.cls"},
		// The platform's.
		{Src: cls + "svc/CartService.cls", Dst: cls + "util/Logger.cls:Logger.debug"},
		{Src: cls + "svc/CartService.cls", Dst: cls + "util/Logger.cls:Logger.insert"},
		{Src: cls + "svc/CartServiceTest.cls", Dst: cls + "util/Logger.cls:Logger.startTest"},
		{Src: cls + "svc/CartServiceTest.cls", Dst: cls + "util/Logger.cls:Logger.assertEquals"},
		{Src: cls + "handlers/AccountHandler.cls", Dst: cls + "util/Logger.cls:Logger.insert"},
		// Another class's `rate`, which the variable's stated type rules out.
		{Src: cls + "svc/CartService.cls", Dst: cls + "tax/VatCalc.cls"},
		{Src: cls + "svc/CartService.cls:CartService.checkout", Dst: cls + "util/Money.cls:Money.rate"},
	}
	extraOK := []want{
		{cls + "svc/CartService.cls:CartService.checkout", cls + "svc/CartService.cls:CartService.audit", model.ConfidenceLow},
		{cls + "util/Money.cls:Money.zero", cls + "util/Money.cls:Money", high},
		// An inner class named by its outer, and the one `Right` also has not taken.
		{cls + "ui/Page.cls:Page.show", cls + "ui/Left.cls:Left.Helper", high},
		{cls + "ui/Page.cls:Page.show", cls + "ui/Left.cls:Left.Helper.run", high},
		{cls + "ui/Page.cls:Page.show", cls + "util/Money.cls:Money.zero", high},
		// A call on what `zero()` returned, which the source does not state.
		{cls + "ui/Page.cls:Page.show", cls + "util/Money.cls:Money.plus", model.ConfidenceLow},
		// A parameter, a local and a return type of a class are uses of it.
		{cls + "util/Money.cls:Money.plus", cls + "util/Money.cls:Money", high},
		{cls + "tax/TaxCalc.cls:TaxCalc.rate", cls + "util/Money.cls:Money", high},
		{cls + "tax/VatCalc.cls:VatCalc.rate", cls + "util/Money.cls:Money", high},
		{cls + "handlers/AccountHandler.cls:AccountHandler.onInsert", cls + "util/Money.cls:Money", high},
		{cls + "svc/CartServiceTest.cls:CartServiceTest.testCheckout", cls + "util/Money.cls:Money", high},
		{cls + "handlers/AccountHandler.cls:AccountHandler.onInsert", cls + "util/Logger.cls:Logger", high},
	}
	scoreEdges(t, got, wants, forbid, extraOK)
}

func apexFixture() map[string]string {
	const cls = "force-app/main/default/classes/"
	return map[string]string{
		"sfdx-project.json": `{"packageDirectories": [{"path": "force-app", "default": true}]}
`,
		cls + "util/Money.cls": `public class Money {
    public Decimal amount;

    public static Money zero() {
        return new Money();
    }

    public Money plus(Money other) {
        return this;
    }

    public Decimal rate() {
        return 0;
    }
}
`,
		cls + "util/Logger.cls": `public class Logger {
    public static void info(String msg) {
        System.debug(msg);
    }

    public static void debug(String msg) {}

    public static void insert(String msg) {}

    public static void startTest() {}

    public static void assertEquals(Object a, Object b) {}
}
`,
		cls + "tax/TaxCalc.cls": `public class TaxCalc {
    public Decimal rate(Money m) {
        return 0.2;
    }
}
`,
		cls + "tax/VatCalc.cls": `public class VatCalc {
    public Decimal rate(Money m) {
        return 0.1;
    }
}
`,
		cls + "svc/IService.cls": `public interface IService {
    void audit();
}
`,
		cls + "svc/BaseService.cls": `public virtual class BaseService {
    public virtual void audit() {}

    protected void log(String m) {}
}
`,
		cls + "svc/CartService.cls": `public with sharing class CartService extends BaseService implements IService {
    public CartService() {
        super();
    }

    public static Money checkout(List<Account> accts) {
        Money m = Money.zero();
        TaxCalc t = new TaxCalc();
        t.rate(m);
        m.plus(m);
        Logger.info('x');
        System.debug('checkout');
        Database.insert(accts);
        this.audit();
        log('done');
        audit();
        return m;
    }

    public override void audit() {
        super.audit();
    }
}
`,
		cls + "handlers/AccountHandler.cls": `public class AccountHandler {
    public static void onInsert(List<Account> accts) {
        Money m = Money.zero();
        CartService.checkout(accts);
        Logger l = new Logger();
        System.debug(m);
        Database.insert(accts);
    }
}
`,
		cls + "svc/CartServiceTest.cls": `@isTest
private class CartServiceTest {
    @isTest
    static void testCheckout() {
        Test.startTest();
        CartService svc = new CartService();
        Money m = Money.zero();
        CartService.checkout(new List<Account>());
        Test.stopTest();
        System.assertEquals(1, 1);
    }
}
`,
		// Two outer classes with an inner class of one name.
		cls + "ui/Left.cls": `public class Left {
    public class Helper {
        public void run() {}
    }
}
`,
		cls + "ui/Right.cls": `public class Right {
    public class Helper {
        public void run() {}
    }
}
`,
		cls + "ui/Page.cls": `public class Page {
    public void show(Left.Helper h) {
        h.run();
        Money.zero().plus(null);
        Left.Helper lh = new Left.Helper();
        lh.run();
    }
}
`,
		"force-app/main/default/triggers/AccountTrigger.trigger": `trigger AccountTrigger on Account (before insert) {
    AccountHandler.onInsert(Trigger.new);
}
`,
	}
}
