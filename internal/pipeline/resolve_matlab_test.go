package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestMatlabResolution indexes a small analysis toolbox (function files, local
// functions, package folders, an @Class folder, a private folder, a classdef and its
// test, and a script) and checks every call against how MATLAB finds a function: by
// its file's name, from any folder on the path.
//
// What it is built to catch: a local function (`trimOutliers` in summarize.m) that a
// file of the same name elsewhere must not replace; `computeMean`, which two folders
// define, where a call is bound to the nearest and never above low unless the
// caller's own folder has one; a package function that a bare call does not reach
// (`render` is `+report/render.m` and `@Legacy/render.m`, a method) unless the file
// imported the package (`import report.*`); `report.write(x)` and
// `tools.stats.zscore(x)`, qualified by package folders; a `private/` function that
// only the folder above calls; a classdef constructor (`Account(10)`) and a static
// method (`Account.make(3)`), while `acct.deposit(5)` on a value binds to nothing; a
// variable that is indexed like a call (`mean(2)` after `mean = ...`, `y(3) = f`);
// `@scaleValue`, a function handle; and the builtins (`disp`, `sum`, `fprintf`,
// `plot`, `figure`, `strcat`).
//
// What it does not bind, by design: a method called on an object, a function of a
// toolbox that is not in the repository, and a name that the same script both
// calls and assigns.
func TestMatlabResolution(t *testing.T) {
	got := langEdges(t, matlabFixture())
	high, med, low := model.ConfidenceHigh, model.ConfidenceMedium, model.ConfidenceLow
	wants := []want{
		// The file's own functions, local and primary.
		{"src/analysis/runAnalysis.m:runAnalysis", "src/analysis/runAnalysis.m:helperOnlyHere", high},
		{"src/analysis/summarize.m:summarize", "src/analysis/summarize.m:trimOutliers", high},
		// By file name, from another folder; handles and calls alike.
		{"src/analysis/runAnalysis.m:runAnalysis", "src/util/normalizeData.m:normalizeData", high},
		{"src/analysis/runAnalysis.m:runAnalysis", "src/analysis/summarize.m:summarize", high},
		{"src/analysis/runAnalysis.m:runAnalysis", "src/analysis/formatReport.m:formatReport", high},
		{"src/analysis/runAnalysis.m:runAnalysis", "src/plots/plotTrend.m:plotTrend", high},
		{"src/analysis/runAnalysis.m:runAnalysis", "src/util/scaleValue.m:scaleValue", high},
		{"src/analysis/runAnalysis.m:helperOnlyHere", "src/analysis/summarize.m:summarize", high},
		{"src/analysis/formatReport.m:formatReport", "src/util/padLeft.m:padLeft", high},
		{"src/analysis/formatReport.m:formatReport", "src/util/logMessage.m:logMessage", high},
		{"src/plots/plotTrend.m:plotTrend", "src/util/padLeft.m:padLeft", high},
		{"scripts/run_all.m:(top level)", "src/analysis/runAnalysis.m:runAnalysis", high},
		{"scripts/run_all.m:(top level)", "src/util/scaleValue.m:scaleValue", high},
		// Two folders define computeMean: the caller's own, medium; the nearest otherwise, low.
		{"src/util/normalizeData.m:normalizeData", "src/util/computeMean.m:computeMean", med},
		{"src/analysis/runAnalysis.m:runAnalysis", "src/util/computeMean.m:computeMean", low},
		{"src/analysis/summarize.m:summarize", "src/util/computeMean.m:computeMean", low},
		{"src/+tools/+stats/zscore.m:zscore", "src/util/computeMean.m:computeMean", low},
		// Package folders: qualified, and imported.
		{"src/analysis/runAnalysis.m:runAnalysis", "src/+report/write.m:write", high},
		{"src/analysis/runAnalysis.m:runAnalysis", "src/+ioutil/save.m:save", high},
		{"src/analysis/runAnalysis.m:runAnalysis", "src/+tools/+stats/zscore.m:zscore", high},
		{"src/+ioutil/save.m:save", "src/util/logMessage.m:logMessage", high},
		{"scripts/run_all.m:(top level)", "src/+report/write.m:write", high},
		{"scripts/run_all.m:(top level)", "src/+report/render.m:render", high},
		// A private folder is seen by the folder above it.
		{"src/util/useSecret.m:useSecret", "src/util/private/secret.m:secret", high},
		// A class: its constructor, a static method, its own methods, a test.
		{"src/analysis/runAnalysis.m:runAnalysis", "src/classes/Account.m:Account.Account", high},
		{"src/analysis/runAnalysis.m:runAnalysis", "src/classes/Account.m:Account.make", high},
		{"src/classes/Account.m:Account.make", "src/classes/Account.m:Account.Account", high},
		{"src/classes/Account.m:Account.deposit", "src/classes/Account.m:Account.audit", high},
		{"src/classes/Account.m:Account.Account", "src/util/logMessage.m:logMessage", high},
		{"src/classes/Account.m:Account.audit", "src/util/logMessage.m:logMessage", high},
		{"tests/AccountTest.m:AccountTest.testDeposit", "src/classes/Account.m:Account.Account", high},
		{"tests/AccountTest.m:AccountTest.testDeposit", "src/analysis/summarize.m:summarize", high},
	}
	forbid := []want{
		// The toolbox's computeMean is never the nearest.
		{Src: "src/", Dst: "toolbox/"},
		// A local function is not a function of the path, and a file of the same name is not it.
		{Src: "src/analysis/summarize.m:summarize", Dst: "src/analysis/trimOutliers.m"},
		// A private function is not seen from another folder.
		{Src: "src/plots/plotTrend.m", Dst: "src/util/private/secret.m"},
		// A package function is not a bare name, nor is a method of an @Class folder.
		{Src: "src/+report/write.m:write", Dst: "src/+report/render.m"},
		{Src: "scripts/run_all.m", Dst: "src/@Legacy/render.m"},
		{Src: "src/", Dst: "src/@Legacy/"},
		// A method called on a value, and a variable that is indexed.
		{Src: "src/analysis/runAnalysis.m", Dst: ":Account.deposit"},
		{Src: "tests/AccountTest.m", Dst: ":Account.deposit"},
		{Src: "src/analysis/runAnalysis.m", Dst: ":mean"},
		{Src: "scripts/run_all.m:(top level)", Dst: "computeMean"},
	}
	scoreEdges(t, got, wants, forbid, nil)
	capped(t, got, []want{
		{Src: "src/analysis/", Dst: "computeMean", Min: low},
		{Src: "src/+tools/", Dst: "computeMean", Min: low},
	})
}

func matlabFixture() map[string]string {
	return map[string]string{
		"src/analysis/runAnalysis.m": `function result = runAnalysis(data)
  clean = normalizeData(data);
  stats = summarize(clean);
  result = formatReport(stats);
  disp(result);
  total = sum(clean);
  mean = computeMean(clean);
  fprintf('%f\n', mean(2));
  helperOnlyHere(total);
  plotTrend(clean);
  cellfun(@scaleValue, {1, 2});
  report.write(result);
  acct = Account(10);
  acct.deposit(5);
  Account.make(3);
  ioutil.save(result);
  tools.stats.zscore(clean);
end

function helperOnlyHere(x)
  disp(x);
  summarize(x);
end
`,
		"src/analysis/summarize.m": `function s = summarize(x)
  s = struct('n', numel(x), 'm', computeMean(x));
  s.extra = trimOutliers(x);
end

function out = trimOutliers(x)
  out = x(abs(x) < 3);
end
`,
		"src/analysis/trimOutliers.m": `function out = trimOutliers(x)
  out = x;
end
`,
		"src/analysis/formatReport.m": `function txt = formatReport(stats)
  txt = sprintf('n=%d', stats.n);
  txt = strcat(txt, padLeft(txt));
  logMessage(txt);
end
`,
		"src/util/normalizeData.m": `function y = normalizeData(x)
  y = (x - computeMean(x)) ./ std(x);
end
`,
		"src/util/computeMean.m": `function m = computeMean(x)
  m = sum(x) / numel(x);
end
`,
		"src/util/padLeft.m": `function p = padLeft(s)
  p = [' ' s];
end
`,
		"src/util/logMessage.m": `function logMessage(msg)
  fprintf('%s\n', msg);
end
`,
		"src/util/scaleValue.m": `function v = scaleValue(x)
  v = x * 2;
end
`,
		"toolbox/computeMean.m": `function m = computeMean(x)
  m = mean(x);
end
`,
		"src/util/private/secret.m": `function secret()
end
`,
		"src/util/useSecret.m": `function useSecret()
  secret();
end
`,
		"src/plots/plotTrend.m": `function plotTrend(y)
  figure;
  plot(y);
  secret();
  padLeft('x');
end
`,
		"src/+report/write.m": `function write(txt)
  fprintf('%s\n', txt);
  render(txt);
end
`,
		"src/+report/render.m": `function render(txt)
  disp(txt);
end
`,
		"src/+ioutil/save.m": `function save(x)
  logMessage(x);
end
`,
		"src/+tools/+stats/zscore.m": `function z = zscore(x)
  z = (x - computeMean(x));
end
`,
		"src/@Legacy/Legacy.m": `function obj = Legacy(v)
  obj.v = v;
  obj = class(obj, 'Legacy');
end
`,
		"src/@Legacy/render.m": `function render(obj)
  disp(obj.v);
end
`,
		"src/classes/Account.m": `classdef Account < handle
  properties
    balance = 0;
  end
  methods
    function obj = Account(b)
      obj.balance = b;
      logMessage('open');
    end
    function deposit(obj, amt)
      obj.balance = obj.balance + amt;
      audit(obj, amt);
    end
    function audit(obj, amt)
      logMessage(num2str(amt));
    end
  end
  methods (Static)
    function r = make(n)
      r = Account(n);
    end
  end
end
`,
		"tests/AccountTest.m": `classdef AccountTest < matlab.unittest.TestCase
  methods (Test)
    function testDeposit(testCase)
      a = Account(1);
      a.deposit(2);
      testCase.verifyEqual(a.balance, 3);
      summarize([1 2 3]);
    end
  end
end
`,
		"scripts/run_all.m": `data = rand(10, 1);
out = runAnalysis(data);
import report.*
write(out);
render(out);
x = computeMean(out);
y(3) = scaleValue(2);
computeMean = 4;
z = computeMean(1);
`,
	}
}
