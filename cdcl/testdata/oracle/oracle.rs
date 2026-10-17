//! Differential-testing oracle: solves problems read from $ORACLE_IN (JSON)
//! with the BundleBox provider and writes the results to $ORACLE_OUT.
use super::*;
use serde_json::Value;
use std::fmt::Write as _;

fn strs(v: &Value) -> Vec<String> {
    v.as_array().map(|a| a.iter().map(|x| x.as_str().unwrap().to_string()).collect()).unwrap_or_default()
}

fn solve_one(p: &Value) -> String {
    let mut provider = BundleBoxProvider::new();
    for pkg in p["packages"].as_array().unwrap() {
        let name = pkg["name"].as_str().unwrap();
        let mut pack = Pack::new(pkg["version"].as_u64().unwrap() as u32);
        if pkg["unknown"].as_bool().unwrap_or(false) {
            pack = pack.with_unknown_deps();
        }
        let deps = strs(&pkg["deps"]);
        let cons = strs(&pkg["constrains"]);
        let deps: Vec<&str> = deps.iter().map(String::as_str).collect();
        let cons: Vec<&str> = cons.iter().map(String::as_str).collect();
        provider.add_package(name, pack, &deps, &cons);
    }
    for f in p["favored"].as_array().unwrap() {
        provider.set_favored(f[0].as_str().unwrap(), f[1].as_u64().unwrap() as u32);
    }
    for f in p["locked"].as_array().unwrap() {
        provider.set_locked(f[0].as_str().unwrap(), f[1].as_u64().unwrap() as u32);
    }
    for f in p["excluded"].as_array().unwrap() {
        provider.exclude(f[0].as_str().unwrap(), f[1].as_u64().unwrap() as u32, f[2].as_str().unwrap());
    }
    for f in p["allow_multiple"].as_array().unwrap() {
        provider.set_allow_multiple(f.as_str().unwrap());
    }
    provider.hint_dependencies_available = p["hint_all"].as_bool().unwrap_or(false);
    let reqs = strs(&p["requirements"]);
    let reqs: Vec<&str> = reqs.iter().map(String::as_str).collect();
    let cons = strs(&p["constraints"]);
    let cons: Vec<&str> = cons.iter().map(String::as_str).collect();
    let requirements = provider.requirements(&reqs);
    let constraints = provider.version_sets(&cons);
    let soft: Vec<SolvableId> = p["soft"]
        .as_array()
        .unwrap()
        .iter()
        .map(|s| {
            let mut pack = Pack::new(s[1].as_u64().unwrap() as u32);
            if s[2].as_bool().unwrap_or(false) {
                pack = pack.with_unknown_deps();
            }
            provider.solvable_id(s[0].as_str().unwrap(), pack)
        })
        .collect();
    let mut solver = Solver::new(provider);
    if let Some(a) = p.get("activity").and_then(|a| a.as_array()) {
        solver = solver.with_activity_params(a[0].as_f64().unwrap() as f32, a[1].as_f64().unwrap() as f32);
    }
    let problem = Problem::new()
        .requirements(requirements)
        .constraints(constraints)
        .soft_requirements(soft);
    let mut out = String::new();
    match solver.solve(problem) {
        Ok(solvables) => {
            writeln!(out, "SOLUTION").unwrap();
            for s in &solvables {
                writeln!(out, "{}", solver.provider().display_solvable(*s)).unwrap();
            }
        }
        Err(UnsolvableOrCancelled::Unsolvable(conflict)) => {
            writeln!(out, "UNSAT").unwrap();
            write!(out, "{}", conflict.display_user_friendly(&solver)).unwrap();
            let mut dot = Vec::new();
            conflict.graph(&solver).graphviz(&mut dot, solver.provider(), true).unwrap();
            writeln!(out, "GRAPHVIZ {}", String::from_utf8(dot).unwrap()).unwrap();
        }
        Err(UnsolvableOrCancelled::Cancelled(_)) => {
            writeln!(out, "CANCELLED").unwrap();
        }
    }
    writeln!(out, "CLAUSES {}", solver.clause_count()).unwrap();
    writeln!(out, "DEFERRED {}", solver.deferred_requirements_count()).unwrap();
    out
}

#[test]
fn oracle() {
    let Ok(input) = std::env::var("ORACLE_IN") else { return };
    let output = std::env::var("ORACLE_OUT").unwrap();
    let problems: Value = serde_json::from_str(&std::fs::read_to_string(input).unwrap()).unwrap();
    let mut out = String::new();
    for (i, p) in problems.as_array().unwrap().iter().enumerate() {
        writeln!(out, "=== {i}").unwrap();
        let result = std::panic::catch_unwind(|| solve_one(p));
        match result {
            Ok(s) => out.push_str(&s),
            Err(_) => writeln!(out, "PANIC").unwrap(),
        }
    }
    std::fs::write(output, out).unwrap();
}
