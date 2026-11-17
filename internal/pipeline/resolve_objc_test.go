package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestObjCResolution indexes a small Objective-C application and checks every
// message and C call against what the #imports make it reach.
//
// What it is built to catch: two Pricing classes in different directories where
// only the one the file imports is meant; two classes that both declare
// saveItem: where the receiver's declared class says which; a C function and a
// static helper; and a framework call (`[NSNotificationCenter
// postNotificationName:...]`) whose method name a repository class also
// declares. The last one binds at low, wrongly, and is listed as known: the
// receiver of that message is the value of another message, whose type the
// source does not state.
func TestObjCResolution(t *testing.T) {
	got := langEdges(t, objcFixture())
	high, med := model.ConfidenceHigh, model.ConfidenceMedium
	wants := []want{
		{"Shop/Cart.m:Cart.addItem", "Shop/Cart.m:Cart.recalc", high},
		{"Shop/Cart.m:Cart.addItem", "Shop/Cart.m:helper", high},
		{"Shop/Cart.m:Cart.addItem", "Shop/Models/Item.h:Item.initWithName", high},
		{"Shop/Cart.m:Cart.addItem", "Shop/Pricing/Pricing.h:Pricing.discountFor", high},
		{"Shop/Cart.m:Cart.addItem", "Shop/Pricing/Pricing.m:taxRate", high},
		{"Shop/Cart.m:Cart.addItem", "Shop/Store/Store.h:Store.saveItem", high},
		// Item.h reaches Pricing.m only through Pricing.h: a type found by its
		// name alone is a preference.
		{"Shop/Pricing/Pricing.m:Pricing.discountFor", "Shop/Models/Item.h:Item.price", med},
		{"ShopTests/CartTests.m:CartTests.testAdd", "Shop/Cart.h:Cart.recalc", high},
		{"ShopTests/CartTests.m:CartTests.testAdd", "Shop/Cart.h:Cart.sharedCart", high},
	}
	forbid := []want{
		{Src: "Shop/", Dst: "Vendor/"},
		{Src: "Shop/Cart.m", Dst: "Shop/Store/Cache"},
	}
	scoreEdges(t, got, wants, forbid, nil,
		want{Src: "Shop/Cart.m:Cart.addItem", Dst: "Shop/Events/Events.h:Events.postNotificationName"})
}

func objcFixture() map[string]string {
	return map[string]string{
		"Shop/Models/Item.h": `#import <Foundation/Foundation.h>

@interface Item : NSObject
@property (nonatomic, copy) NSString *name;
- (instancetype)initWithName:(NSString *)name;
- (double)price;
@end
`,
		"Shop/Models/Item.m": `#import "Item.h"

@implementation Item
- (instancetype)initWithName:(NSString *)name {
    self = [super init];
    if (self) {
        _name = name;
    }
    return self;
}
- (double)price { return 1.0; }
@end
`,
		"Shop/Pricing/Pricing.h": `#import <Foundation/Foundation.h>
#import "Item.h"

@interface Pricing : NSObject
+ (double)discountFor:(Item *)item;
@end

double taxRate(void);
`,
		"Shop/Pricing/Pricing.m": `#import "Pricing.h"

@implementation Pricing
+ (double)discountFor:(Item *)item {
    return [item price] * 0.9;
}
@end

double taxRate(void) { return 0.2; }
`,
		"Vendor/Pricing/Pricing.h": `#import <Foundation/Foundation.h>

@interface Pricing : NSObject
+ (double)discountFor:(id)thing;
@end
`,
		"Vendor/Pricing/Pricing.m": `#import "Pricing.h"

@implementation Pricing
+ (double)discountFor:(id)thing { return 0; }
@end
`,
		"Shop/Store/Store.h": `#import <Foundation/Foundation.h>
#import "Item.h"

@interface Store : NSObject
- (void)saveItem:(Item *)item;
@end
`,
		"Shop/Store/Store.m": `#import "Store.h"

@implementation Store
- (void)saveItem:(Item *)item { }
@end
`,
		"Shop/Store/Cache.h": `#import <Foundation/Foundation.h>
#import "Item.h"

@interface Cache : NSObject
- (void)saveItem:(Item *)item;
@end
`,
		"Shop/Store/Cache.m": `#import "Cache.h"

@implementation Cache
- (void)saveItem:(Item *)item { }
@end
`,
		"Shop/Events/Events.h": `#import <Foundation/Foundation.h>

@interface Events : NSObject
- (void)postNotificationName:(NSString *)name object:(id)object;
@end
`,
		"Shop/Events/Events.m": `#import "Events.h"

@implementation Events
- (void)postNotificationName:(NSString *)name object:(id)object { }
@end
`,
		"Shop/Cart.h": `#import <Foundation/Foundation.h>
#import "Item.h"

@interface Cart : NSObject
- (void)addItem:(Item *)item quantity:(NSInteger)q;
- (void)recalc;
+ (instancetype)sharedCart;
@end
`,
		"Shop/Cart.m": `#import "Cart.h"
#import "Models/Item.h"
#import "Pricing/Pricing.h"
#import "Store/Store.h"
@import UIKit;

static double helper(double x) { return x; }

@implementation Cart

- (void)addItem:(Item *)item quantity:(NSInteger)q {
    double x = [Pricing discountFor:item];
    Store *store = [[Store alloc] init];
    [store saveItem:item];
    Item *y = [[Item alloc] initWithName:@"a"];
    [self recalc];
    helper(x);
    double t = taxRate();
    NSLog(@"total %f", x + t);
    [[NSNotificationCenter defaultCenter] postNotificationName:@"added" object:nil];
}

+ (instancetype)sharedCart { return nil; }
- (void)recalc { }
@end
`,
		"ShopTests/CartTests.m": `#import <XCTest/XCTest.h>
#import "Cart.h"

@interface CartTests : XCTestCase
@end

@implementation CartTests
- (void)testAdd {
    Cart *cart = [Cart sharedCart];
    [cart recalc];
}
@end
`,
	}
}
