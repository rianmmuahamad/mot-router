CXX=/usr/bin/g++-10
CXXFLAGS=-std=c++17 -O2 -pthread -Wall
LDLIBS=-lcurl
SRC=$(wildcard src/*.cpp)
OBJ=$(SRC:.cpp=.o)

all: ipfarm

.PHONY: all clean

ipfarm: $(OBJ)
	$(CXX) $(CXXFLAGS) -o $@ $(OBJ) $(LDLIBS)

%.o: %.cpp
	$(CXX) $(CXXFLAGS) -c $< -o $@

clean:
	rm -f $(OBJ) ipfarm
