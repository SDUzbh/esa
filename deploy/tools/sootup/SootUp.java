package esa.tools;

import java.io.File;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.IdentityHashMap;
import java.util.List;
import sootup.core.graph.ControlFlowGraph;
import sootup.core.inputlocation.AnalysisInputLocation;
import sootup.core.jimple.common.stmt.Stmt;
import sootup.core.model.SourceType;
import sootup.java.bytecode.frontend.inputlocation.JavaClassPathAnalysisInputLocation;
import sootup.java.core.JavaSootMethod;
import sootup.java.core.views.JavaView;

/** Small read-only entry point; the complete SDK is also available to scratch tools. */
public final class SootUp {
    private static String classpath() throws Exception {
        List<String> paths = new ArrayList<>();
        paths.add("/input/classes");
        for (String directory : List.of("/input/deps", "/opt/java/openjdk/jre/lib", "/opt/java/openjdk/jre/lib/ext")) {
            if (Files.isDirectory(Path.of(directory))) {
                try (var files = Files.list(Path.of(directory))) {
                    files.filter(p -> p.toString().endsWith(".jar")).sorted().forEach(p -> paths.add(p.toString()));
                }
            }
        }
        return String.join(File.pathSeparator, paths);
    }

    public static void main(String[] args) throws Exception {
        if (args.length == 1 && args[0].equals("--version")) {
            System.out.println("SootUp " + Files.readString(Path.of("/opt/esa/sootup/version.txt")).trim());
            return;
        }
        if (args.length == 0 || args[0].equals("--help")) {
            System.out.println("Usage: sootup jimple|cfg CLASS [METHOD] [--classpath PATHS]\n"
                + "Default input: frozen product classes, dependencies and target JDK 8 libraries.\n"
                + "CFG output includes normal and exceptional edges. Redirect output to /workspace.\n"
                + "SDK/callgraph libraries: /opt/esa/sootup/lib/*; analysis JDK: /opt/esa/tool-jdk.");
            return;
        }
        if (args.length < 2 || !(args[0].equals("jimple") || args[0].equals("cfg"))) {
            throw new IllegalArgumentException("Expected jimple|cfg CLASS [METHOD]; see --help");
        }
        String method = null;
        String cp = classpath();
        for (int i = 2; i < args.length; i++) {
            if (args[i].equals("--classpath") && i + 1 < args.length) cp = args[++i];
            else if (method == null && !args[i].startsWith("--")) method = args[i];
            else throw new IllegalArgumentException("Unexpected argument: " + args[i]);
        }
        AnalysisInputLocation input = new JavaClassPathAnalysisInputLocation(cp, SourceType.Application);
        JavaView view = new JavaView(input);
        var type = view.getIdentifierFactory().getClassType(args[1]);
        var clazz = view.getClass(type).orElseThrow(() -> new IllegalArgumentException("Class not found: " + args[1]));
        List<JavaSootMethod> methods = new ArrayList<>(clazz.getMethods());
        methods.sort(Comparator.comparing(m -> m.getSignature().toString()));
        boolean matched = false;
        for (JavaSootMethod candidate : methods) {
            if (method != null && !candidate.getName().equals(method) && !candidate.getSignature().toString().equals(method)) continue;
            matched = true;
            System.out.println("// " + candidate.getSignature());
            if (candidate.isAbstract() || candidate.isNative()) continue;
            var body = candidate.getBody();
            if (args[0].equals("jimple")) System.out.println(body);
            else {
                ControlFlowGraph<?> graph = body.getControlFlowGraph();
                List<Stmt> statements = body.getStmts();
                var ids = new IdentityHashMap<Stmt, Integer>();
                for (int i = 0; i < statements.size(); i++) ids.put(statements.get(i), i);
                for (int i = 0; i < statements.size(); i++) {
                    Stmt statement = statements.get(i);
                    System.out.println(i + ": " + statement);
                    for (Stmt target : graph.successors(statement)) System.out.println("  -> " + ids.get(target));
                    graph.exceptionalSuccessors(statement).forEach((exception, target) ->
                        System.out.println("  ! " + exception + " -> " + ids.get(target)));
                }
            }
        }
        if (!matched) throw new IllegalArgumentException("Method not found: " + method);
    }
}
