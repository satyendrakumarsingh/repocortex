package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestSolidityResolution indexes a small Foundry project and checks every call
// against what its imports make it reach.
//
// What it is built to catch: `SafeMath.add()` where two libraries share a name
// and only one is imported; the three import forms (`import "./A.sol"`,
// `import {A, B as C} from`, `import * as L from`); `this.f()`, `super.f()` and
// a function inherited through `contract B is A`; `new Vault()`; `require`,
// `keccak256`, `abi.encode` and `msg.sender.call`, which are the language's and
// not the `require`, `encode` and `call` of a contract of the repository; a
// contract of the same directory that was not imported; and a Foundry test.
func TestSolidityResolution(t *testing.T) {
	got := langEdges(t, solidityFixture())
	high := model.ConfidenceHigh
	wants := []want{
		// A library reached by a named import, a renamed one, and a namespace.
		{"src/Token.sol:Token.mint", "src/lib/SafeMath.sol:SafeMath.add", high},
		{"src/Token.sol:Token.burn", "src/lib/Checks.sol:Checks.positive", high},
		{"src/Token.sol:Token.burn", "src/lib/Fees.sol:Fees.rate", high},
		// A plain import brings in every global of the file.
		{"src/Token.sol:Token.mint", "src/Ownable.sol:Ownable.onlyOwner", high},
		// `this.f()`, a bare call, an inherited one, and `super.f()`.
		{"src/Token.sol:Token.mint", "src/Token.sol:Token.balance", high},
		{"src/Token.sol:Token.burn", "src/Token.sol:Token.balance", high},
		{"src/Token.sol:Token.mint", "src/Ownable.sol:Ownable.owner", high},
		{"src/Token.sol:Token.pause", "src/Base.sol:Base.pause", high},
		// A contract constructed, and one a call is made on through a typed variable.
		{"src/Token.sol:Token.deploy", "src/Vault.sol:Vault", high},
		{"src/Token.sol:Token.deploy", "src/Vault.sol:Vault.deposit", high},
		// The `is` clause.
		{"src/Token.sol:Token", "src/Ownable.sol:Ownable", high},
		{"src/Token.sol:Token", "src/Base.sol:Base", high},
		// An event and a modifier of the contract itself.
		{"src/Token.sol:Token.mint", "src/Token.sol:Token.Minted", high},
		// A Foundry test: `token` is a state variable of the type Token, which the
		// call is on by the variable's name (medium), and the test constructs it.
		{"test/Token.t.sol:TokenTest.setUp", "src/Token.sol:Token", high},
		{"test/Token.t.sol:TokenTest.testMint", "src/Token.sol:Token.mint", model.ConfidenceMedium},
		{"test/Token.t.sol:TokenTest.testMint", "src/Token.sol:Token.balance", model.ConfidenceMedium},
	}
	forbid := []want{
		// A library of the same name that the file did not import, and a contract
		// of the same directory that it did not.
		{Src: "src/Token.sol", Dst: "src/old/SafeMath.sol"},
		{Src: "src/Token.sol", Dst: "src/Vault2.sol"},
		{Src: "src/Token.sol", Dst: "src/Unused.sol"},
		// The language's own.
		{Src: "src/Token.sol", Dst: "src/Helpers.sol:Helpers.require"},
		{Src: "src/Token.sol", Dst: "src/Helpers.sol:Helpers.encode"},
		{Src: "src/Token.sol", Dst: "src/Helpers.sol:Helpers.call"},
		{Src: "src/Token.sol", Dst: "src/Helpers.sol:Helpers.keccak256"},
		{Src: "test/Token.t.sol", Dst: "src/Helpers.sol"},
	}
	extraOK := []want{
		// A contract holds and passes the libraries and contracts it uses.
		{"src/Token.sol:Token", "src/lib/SafeMath.sol:SafeMath", high},
		{"src/Token.sol:Token.deploy", "src/Vault.sol:Vault", high},
		{"src/Token.sol:Token.constructor", "src/Base.sol:Base", high},
		{"test/Token.t.sol:TokenTest.token", "src/Token.sol:Token", high},
	}
	// `to` is an address, whose `transfer` is the language's, and the one contract
	// of the repository with a member of that name is bound to it, at low: the
	// receiver's type is a value type the source states and the index does not
	// read. It is wrong, and is counted.
	known := []want{{"src/Token.sol:Token.pay", "src/Vault.sol:Vault.transfer", model.ConfidenceLow}}
	scoreEdges(t, got, wants, forbid, extraOK, known...)
}

func solidityFixture() map[string]string {
	return map[string]string{
		"foundry.toml": `[profile.default]
src = "src"
test = "test"
`,
		"src/lib/SafeMath.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

library SafeMath {
    function add(uint256 a, uint256 b) internal pure returns (uint256) {
        return a + b;
    }
}
`,
		"src/old/SafeMath.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

library SafeMath {
    function add(uint256 a, uint256 b) internal pure returns (uint256) {
        return a + b;
    }
}
`,
		"src/lib/Checks.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

library Checks {
    function positive(uint256 a) internal pure returns (bool) {
        return a > 0;
    }
}
`,
		"src/lib/Fees.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

library Fees {
    function rate() internal pure returns (uint256) {
        return 3;
    }
}
`,
		"src/Ownable.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

contract Ownable {
    address private _owner;

    modifier onlyOwner() {
        require(msg.sender == _owner, "not owner");
        _;
    }

    function owner() public view returns (address) {
        return _owner;
    }
}
`,
		"src/Base.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

contract Base {
    bool internal paused;

    function pause() public virtual {
        paused = true;
    }
}
`,
		"src/Vault.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

contract Vault {
    function deposit(uint256 a) public {}

    function transfer(address to, uint256 a) public {}
}
`,
		"src/Vault2.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

contract Vault2 {
    function deposit(uint256 a) public {}
}
`,
		"src/Unused.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

contract Unused {
    function mint(uint256 a) public {}
}
`,
		// A contract with the names the language owns.
		"src/Helpers.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

contract Helpers {
    function require(bool c) public {}
    function encode(uint256 a) public {}
    function call(address a) public {}
    function keccak256(bytes memory b) public {}
}
`,
		"src/Token.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {SafeMath} from "./lib/SafeMath.sol";
import {Checks, Fees as FeeTable} from "./lib/Checks.sol";
import * as Fees from "./lib/Fees.sol";
import "./Ownable.sol";
import {Base} from "./Base.sol";
import {Vault} from "./Vault.sol";

contract Token is Ownable, Base {
    event Minted(address to, uint256 amount);

    mapping(address => uint256) private balances;

    constructor() Base() {}

    function balance(address a) public view returns (uint256) {
        return balances[a];
    }

    function mint(address to, uint256 amount) external onlyOwner {
        require(amount > 0, "zero");
        balances[to] = SafeMath.add(balances[to], amount);
        bytes32 h = keccak256(abi.encode(to, amount));
        (bool ok, ) = msg.sender.call("");
        this.balance(to);
        owner();
        emit Minted(to, amount);
    }

    function burn(uint256 amount) external {
        Checks.positive(amount);
        Fees.Fees.rate();
        balance(msg.sender);
    }

    function pay(address payable to) external {
        to.transfer(1);
    }

    function pause() public override {
        super.pause();
    }

    function deploy() external returns (Vault) {
        Vault v = new Vault();
        v.deposit(1);
        return v;
    }
}
`,
		"test/Token.t.sol": `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "forge-std/Test.sol";
import {Token} from "../src/Token.sol";

contract TokenTest is Test {
    Token token;

    function setUp() public {
        token = new Token();
    }

    function testMint() public {
        vm.prank(address(1));
        token.mint(address(2), 5);
        assertEq(token.balance(address(2)), 5);
    }
}
`,
	}
}
